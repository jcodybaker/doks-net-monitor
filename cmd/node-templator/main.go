package main

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	flag "github.com/spf13/pflag"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/shirou/gopsutil/v3/process"
)

// NodeTemplatingController renders a template when a node is added or removed.
type NodeTemplatingController struct {
	informerFactory informers.SharedInformerFactory
	nodeInformer    coreinformers.NodeInformer
	template        *template.Template
	lastContents    []byte
}

// Run starts shared informers and waits for the shared informer cache to
// synchronize.
func (c *NodeTemplatingController) Run(stopCh chan struct{}) error {
	// Starts all the shared informers that have been created by the factory so
	// far.
	c.informerFactory.Start(stopCh)
	// wait for the initial synchronization of the local cache.
	if !cache.WaitForCacheSync(stopCh, c.nodeInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync")
	}
	return nil
}

func (c *NodeTemplatingController) nodeAdd(obj interface{}) {
	node := obj.(*v1.Node)
	log.Info().Str("node", node.Name).
		Str("node_phase", string(node.Status.Phase)).
		Msg("node created")
	c.render()
}

func (c *NodeTemplatingController) nodeUpdate(old, new interface{}) {
	newNode := new.(*v1.Node)
	log.Info().
		Str("node", newNode.Name).
		Str("node_phase", string(newNode.Status.Phase)).
		Msg("node updated")
	c.render()
}

func (c *NodeTemplatingController) nodeDelete(obj interface{}) {
	node := obj.(*v1.Node)
	log.Info().
		Str("node", node.Name).
		Msg("node deleted")
	c.render()
}

func (c *NodeTemplatingController) render() {
	nodes, err := c.nodeInformer.Lister().List(labels.Everything())
	if err != nil {
		log.Err(err).Msg("failed to list nodes")
		return
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	b := &bytes.Buffer{}
	if err := c.template.Execute(b, struct {
		Nodes []*v1.Node
	}{Nodes: nodes}); err != nil {
		log.Err(err).Msg("failed to render template")
		return
	}

	if bytes.Equal(b.Bytes(), c.lastContents) {
		log.Debug().Msg("template contents unchanged")
		return
	}

	ll := log.With().Str("output", outFile).Logger()
	// Try to atomically swap the file by writing to a temp file and moving it into place.
	dir := filepath.Dir(outFile)
	file := filepath.Base(outFile)
	f, err := os.CreateTemp(dir, file)
	if err != nil {
		ll.Err(err).Msg("failed to create output file")
		return
	}
	defer f.Close()
	if _, err := f.Write(b.Bytes()); err != nil {
		ll.Err(err).Msg("failed to write to output file")
		return
	}

	if err := os.Rename(f.Name(), outFile); err != nil {
		ll.Err(err).Msg("failed to install output file")
		return
	}
	c.lastContents = b.Bytes()
	ll.Info().Msg("rendered template")
	c.signal()
}

func (c *NodeTemplatingController) signal() {
	if notifyProcess == "" {
		return
	}
	processes, err := process.Processes()
	if err != nil {
		log.Err(err).Msg("failed to list processes")
		return
	}
	for _, p := range processes {
		n, err := p.Name()
		if err != nil {
			log.Err(err).Msg("failed to fetch process name")
			return
		}
		if n == notifyProcess {
			err = p.SendSignal(syscall.SIGHUP)
			if err != nil {
				log.Err(err).Int("pid", int(p.Pid)).Msg("failed to notify process")
				return
			}
			log.Info().Int("pid", int(p.Pid)).Msg("notified process")
		}
	}
}

// NewNodeTemplatingController creates a NodeTemplatingController
func NewNodeTemplatingController(informerFactory informers.SharedInformerFactory, templateFile string) (*NodeTemplatingController, error) {
	t, err := template.New("").Funcs(template.FuncMap{
		"nodeIP": nodeIP,
	}).ParseFiles(templateFile)
	if err != nil {
		return nil, fmt.Errorf("parsing template file: %w", err)
	}
	for _, tmpl := range t.Templates() {
		if tmpl.Name() == "" {
			continue
		}
		t = tmpl
		break
	}
	nodeInformer := informerFactory.Core().V1().Nodes()

	c := &NodeTemplatingController{
		informerFactory: informerFactory,
		nodeInformer:    nodeInformer,
		template:        t,
	}
	_, err = nodeInformer.Informer().AddEventHandler(
		// Your custom resource event handlers.
		cache.ResourceEventHandlerFuncs{
			// Called on creation
			AddFunc: c.nodeAdd,
			// Called on resource update and every resyncPeriod on existing resources.
			UpdateFunc: c.nodeUpdate,
			// Called on resource deletion.
			DeleteFunc: c.nodeDelete,
		},
	)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func nodeIP(node *v1.Node, ipType string) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == v1.NodeAddressType(ipType) {
			return addr.Address
		}
	}
	return ""
}

var (
	kubeconfig    string
	templateFile  string
	outFile       string
	notifyProcess string
)

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&templateFile, "template", "template.yaml", "absolute path to the template file")
	flag.StringVar(&outFile, "out", "config.yaml", "absolute path to the output file")
	flag.StringVar(&notifyProcess, "notify-process", "", "name of process to notify on update")
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr}).Level(zerolog.DebugLevel)
	zerolog.DurationFieldUnit = time.Second

}

func main() {
	flag.Parse()

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	restConfig, err := config.ClientConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to build Kubernetes client config")
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to build Kubernetes clientset")
	}

	factory := informers.NewSharedInformerFactory(clientset, time.Hour*24)
	controller, err := NewNodeTemplatingController(factory, templateFile)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create node templating controller")
	}

	stop := make(chan struct{})
	defer close(stop)
	err = controller.Run(stop)
	if err != nil {
		log.Fatal().Err(err).Msg("running node templating controller")
	}
}
