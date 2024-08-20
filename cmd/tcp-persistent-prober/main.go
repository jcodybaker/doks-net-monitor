package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/rs/zerolog/log"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig    string
	svcSelectors  []string
	probeInterval time.Duration
)

func init() {
	viper.AutomaticEnv()

	flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	viper.BindPFlag("kubeconfig", flag.Lookup("kubeconfig"))

	flag.StringArray("service-selector", nil, "service selector to probe")
	viper.BindPFlag("service_selector", flag.Lookup("service-selector"))

	flag.String("service-selector-namespace", "", "namespace of the service selector. If not set, all namespaces are used.")
	viper.BindPFlag("service_selector_namespace", flag.Lookup("service-selector-namespace"))

	flag.Duration("probe-interval", 1*time.Second, "interval between probes")
	viper.BindPFlag("probe_interval", flag.Lookup("probe-interval"))

	flag.String("echo-bind-address", ":9061", "address to bind echo server")
	viper.BindPFlag("echo_bind_address", flag.Lookup("echo-bind-address"))

	flag.String("metrics-bind-address", ":9060", "address to bind metrics server")
	viper.BindPFlag("metrics_bind_address", flag.Lookup("metrics-bind-address"))

	flag.String("node-name", "", "name of the node")
	viper.BindPFlag("node_name", flag.Lookup("node-name"))

	flag.String("pod-namespace", "", "namespace of the pod")
	viper.BindPFlag("pod_namespace", flag.Lookup("pod-namespace"))

	flag.String("pod-name", "", "name of the pod")
	viper.BindPFlag("pod_name", flag.Lookup("pod-name"))

	flag.String("log-level", "info", "log level")
	viper.BindPFlag("log_level", flag.Lookup("log-level"))

	zerolog.DurationFieldUnit = time.Second
}

func main() {
	flag.Parse()
	logLevel, err := zerolog.ParseLevel(viper.GetString("log_level"))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to parse log level")
	}
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).Level(logLevel)
	ctx := context.Background()
	ctx = log.Logger.WithContext(ctx)
	var wg sync.WaitGroup

	kubeconfig := viper.GetString("kubeconfig")
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	restConfig, err := config.ClientConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create Kubernetes client config")
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create Kubernetes clientset")
	}

	var node *v1.Node
	if nodeName := viper.GetString("node_name"); nodeName != "" {
		var err error
		node, err = clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			log.Fatal().Err(err).Msg("finding local node info")
		}
		log.Debug().Str("node", node.Name).Msg("loaded local node info")
	}
	if node == nil {
		log.Warn().Msg("NODE_NAME not set or node found")
	}

	var pod *v1.Pod
	if podNamespace, podName := viper.GetString("pod_namespace"), viper.GetString("pod_name"); podNamespace != "" && podName != "" {
		var err error
		pod, err = clientset.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			log.Fatal().Err(err).Msg("finding local pod info")
		}
		log.Debug().Str("node", node.Name).Msg("loaded local pod info")
	}

	sMux := http.NewServeMux()

	r := prometheus.NewRegistry()
	sMux.Handle("/metrics", promhttp.HandlerFor(r, promhttp.HandlerOpts{}))
	tcpMetrics := NewTCPMetrics()
	tcpMetrics.Register(r)

	echoServerAddr := viper.GetString("echo_bind_address")
	log.Info().Str("bind_addr", echoServerAddr).Msg("starting echo server")
	stopEchoServer, err := StartEchoServer(
		ctx,
		echoServerAddr,
		viper.GetString("node_name"),
		fmt.Sprintf("%s/%s", viper.GetString("pod_namespace"), viper.GetString("pod_name")))
	if err != nil {
		log.Fatal().Err(err).Msg("failed to start echo server")
	}

	discovery, err := NewDiscovery(ctx, DiscoveryConfig{
		ServiceSelectors:          viper.GetStringSlice("service_selector"),
		ServiceSelectorsNamespace: viper.GetString("service_selector_namespace"),
		ClientSet:                 clientset,
		LocalNode:                 node,
		LocalPod:                  pod,
		OnTargetAdd: func(target string, metadata TargetMetadata) OnTargetRemove {
			log.Info().
				Str("component", "discovery").
				Str("target", target).
				Str("target_type", metadata.TargetType).
				Str("target_node", metadata.RemoteNode).
				Str("target_pod", metadata.RemotePod).
				Str("local_node", metadata.LocalNode).
				Str("local_pod", metadata.LocalPod).
				Msg("adding target")
			p := NewTCPTarget(viper.GetDuration("probe_interval"), target, tcpMetrics, metadata)
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.Run(ctx)
			}()
			return func() {
				log.Info().
					Str("component", "discovery").
					Str("target", target).
					Str("target_type", metadata.TargetType).
					Str("target_node", metadata.RemoteNode).
					Str("target_pod", metadata.RemotePod).
					Str("local_node", metadata.LocalNode).
					Str("local_pod", metadata.LocalPod).
					Msg("stopping target")
				p.Stop()
			}
		},
	})
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create discovery")
	}

	log.Info().Msg("starting discovery")
	if err := discovery.Start(ctx); err != nil {
		log.Fatal().Err(err).Msg("failed to start discovery")
	}

	var shutdown int64
	sMux.Handle("/ready", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt64(&shutdown) != 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !discovery.Healthy() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	sMux.Handle("/healthy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !discovery.Healthy() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	s := http.Server{
		Addr:    ":9060",
		Handler: sMux,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Str("bind_addr", s.Addr).Msg("starting metrics server")
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("failed to start HTTP server")
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	// wait for shutdown signal
	sig := <-sigChan
	log.Info().Str("signal", sig.String()).Msg("shutting down")
	atomic.StoreInt64(&shutdown, 1)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("failed to shutdown HTTP server")
		}
		log.Info().Str("bind_addr", s.Addr).Msg("metrics server stopped")
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		discovery.Stop()
		log.Info().Msg("discovery stopped")
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopEchoServer()
		log.Info().Msg("echo server stopped")
	}()

	wg.Wait()
	log.Info().Msg("shutdown complete")
}
