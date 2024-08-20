package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"

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
	kubeconfig   string
	svcSelectors []string
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringArrayVar(&svcSelectors, "service-selector", nil, "service selector to probe")
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).Level(zerolog.DebugLevel)
	zerolog.DurationFieldUnit = time.Second
}

func main() {
	flag.Parse()
	ctx := context.Background()
	var wg sync.WaitGroup

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
	if nodeName := os.Getenv("NODE_NAME"); nodeName != "" {
		var err error
		node, err = clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			log.Fatal().Err(err).Msg("finding our node info")
		}
	}
	if node == nil {
		log.Warn().Msg("NODE_NAME not set or node found")
	}

	var pod *v1.Pod
	if podNamespace, podName := os.Getenv("POD_NAMESPACE"), os.Getenv("POD_NAME"); podNamespace != "" && podName != "" {
		var err error
		pod, err = clientset.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			log.Fatal().Err(err).Msg("finding our pod info")
		}
	}

	r := prometheus.NewRegistry()
	http.Handle("/metrics", promhttp.HandlerFor(r, promhttp.HandlerOpts{}))
	tcpMetrics := NewTCPMetrics()
	tcpMetrics.Register(prometheus.DefaultRegisterer)

	stopEchoServer, err := StartEchoServer(ctx, ":9061")
	if err != nil {
		log.Fatal().Err(err).Msg("failed to start echo server")
	}

	discovery, err := NewDiscovery(ctx, DiscoveryConfig{
		ServiceSelectors: svcSelectors,
		ClientSet:        clientset,
		LocalNode:        node,
		LocalPod:         pod,
		OnTargetAdd: func(target string, metadata TargetMetadata) OnTargetRemove {
			p := NewTCPTarget(target, tcpMetrics, metadata)
			wg.Add(1)
			go func() {
				defer wg.Done()
				p.Run(ctx)
			}()
			return p.Stop
		},
	})
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create discovery")
	}

	if err := discovery.Start(ctx); err != nil {
		log.Fatal().Err(err).Msg("failed to start discovery")
	}

	var shutdown int64
	http.Handle("/ready", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	http.Handle("/healthy", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !discovery.Healthy() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	s := http.Server{
		Addr: ":9060",
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
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
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		discovery.Stop()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopEchoServer()
	}()

	wg.Wait()

}
