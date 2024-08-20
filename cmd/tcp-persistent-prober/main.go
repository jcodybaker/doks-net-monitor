package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
		"github.com/rs/zerolog"
)

var (
	kubeconfig string
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
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

	var nodeIP string
	if nodeName := os.Getenv("NODE_NAME"); nodeName != "" {
		node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			log.Fatal().Err(err).Msg("finding our node IP")
		}
		for _, adderr := range node.Status.Addresses {
			if adderr.Type == v1.NodeExternalIP {
				nodeIP = adderr.Address
				break
			}
		}
	}
	if nodeIP == "" {
		log.Warn().Msg("NODE_NAME not set or no external IP found")
	}

	r := prometheus.NewRegistry()
	http.Handle("/metrics", promhttp.HandlerFor(r, promhttp.HandlerOpts{}))
	tcpMetrics := NewTCPMetrics(nodeIP)
	tcpMetrics.Register(prometheus.DefaultRegisterer)

	factory := informers.NewSharedInformerFactory(clientset, time.Hour*24)

	// TODO start echo server
	// TODO register health endpoint

	discovery, err := NewDiscovery(ctx, DiscoveryConfig{
		InformerFactory: factory,
		NodeIP:          nodeIP,
		OnTargetAdd: func(target string) OnTargetRemove {
			p := NewTCPTarget(target, tcpMetrics)
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

	http.ListenAndServe(":9060", nil)
}
