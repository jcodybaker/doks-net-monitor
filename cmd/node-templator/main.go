package main

import (
	"bytes"
	"flag"
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
	klog "k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/logs"

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
	klog.Infof("NODE CREATED: %s/%s", node.Namespace, node.Name)
	c.render()
}

func (c *NodeTemplatingController) nodeUpdate(old, new interface{}) {
	oldNode := old.(*v1.Node)
	newNode := new.(*v1.Node)
	klog.Infof(
		"NODE UPDATED. %s/%s %s",
		oldNode.Namespace, oldNode.Name, newNode.Status.Phase,
	)
	c.render()
}

func (c *NodeTemplatingController) nodeDelete(obj interface{}) {
	node := obj.(*v1.Node)
	klog.Infof("NODE DELETED: %s/%s", node.Namespace, node.Name)
	c.render()
}

func (c *NodeTemplatingController) render() {
	nodes, err := c.nodeInformer.Lister().List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes: %v", err)
		return
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	b := &bytes.Buffer{}
	if err := c.template.Execute(b, struct {
		Nodes []*v1.Node
	}{Nodes: nodes}); err != nil {
		klog.Errorf("Failed to render template: %v", err)
		return
	}

	if bytes.Equal(b.Bytes(), c.lastContents) {
		klog.Info("Template contents unchanged")
		return
	}

	// Try to atomically swap the file by writing to a temp file and moving it into place.
	dir := filepath.Dir(outFile)
	file := filepath.Base(outFile)
	f, err := os.CreateTemp(dir, file)
	if err != nil {
		klog.Errorf("Failed to create output file: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(b.Bytes()); err != nil {
		klog.Errorf("Failed to write to output file: %v", err)
		return
	}

	if err := os.Rename(f.Name(), outFile); err != nil {
		klog.Errorf("Failed to install output file: %v", err)
		return
	}
	c.lastContents = b.Bytes()
	klog.Infof("Rendered template to %s", outFile)
	c.signal()
}

func (c *NodeTemplatingController) signal() {
	if notifyProcess == "" {
		return
	}
	processes, err := process.Processes()
	if err != nil {
		klog.Errorf("Failed to list processes: %v", err)
		return
	}
	for _, p := range processes {
		n, err := p.Name()
		if err != nil {
			klog.Errorf("Failed to fetch process name: %v", err)
			return
		}
		if n == notifyProcess {
			err = p.SendSignal(syscall.SIGHUP)
			if err != nil {
				klog.Errorf("Failed to notify process %d: %v", p.Pid, err)
				return
			}
			klog.Infof("Notified process %d", p.Pid)
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
	t = t.Templates()[1]
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
	flag.StringVar(&kubeconfig, "kubeconfig", filepath.Join(os.Getenv("HOME"), ".kube", "config"), "absolute path to the kubeconfig file")
	flag.StringVar(&templateFile, "template", "template.yaml", "absolute path to the template file")
	flag.StringVar(&outFile, "out", "config.yaml", "absolute path to the output file")
	flag.StringVar(&notifyProcess, "notify-process", "", "name of process to notify on update")
}

func main() {
	flag.Parse()
	logs.InitLogs()
	defer logs.FlushLogs()

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	factory := informers.NewSharedInformerFactory(clientset, time.Hour*24)
	controller, err := NewNodeTemplatingController(factory, templateFile)
	if err != nil {
		klog.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	err = controller.Run(stop)
	if err != nil {
		klog.Fatal(err)
	}
	select {}
}
