package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"

	"k8s.io/client-go/tools/cache"
)

type OnTargetAdd func(target string) OnTargetRemove

type OnTargetRemove func()

type DiscoveryConfig struct {
	ServiceSelectors []string
	InformerFactory  informers.SharedInformerFactory
	OnTargetAdd      OnTargetAdd
	NodeIP           string
}

type Discovery struct {
	svcInformer       coreinformers.ServiceInformer
	endpointsInformer coreinformers.EndpointsInformer
	probes            map[string]map[string]OnTargetRemove
	DiscoveryConfig
	parsedServiceSelectors []labels.Selector
	mutex                  sync.Mutex
}

func NewDiscovery(ctx context.Context, config DiscoveryConfig) (*Discovery, error) {
	d := &Discovery{
		DiscoveryConfig:   config,
		svcInformer:       config.InformerFactory.Core().V1().Services(),
		endpointsInformer: config.InformerFactory.Core().V1().Endpoints(),
	}
	d.svcInformer.Informer().AddEventHandler(d)
	d.endpointsInformer.Informer().AddEventHandler(d)
	for _, svcSelector := range config.ServiceSelectors {
		s, err := labels.Parse(svcSelector)
		if err != nil {
			return nil, fmt.Errorf("failed to parse svc selector %q: %v", svcSelector, err)
		}
		d.parsedServiceSelectors = append(d.parsedServiceSelectors, s)
	}
	return d, nil
}

// Run starts shared informers and waits for the shared informer cache to
// synchronize.
func (d *Discovery) Start(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), d.endpointsInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync svc informer")
	}
	// wait for the initial synchronization of the local cache.
	if !cache.WaitForCacheSync(ctx.Done(), d.svcInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync svc informer")
	}
	// We need to force a resync of the services as the endpoints informer may not have sync'ed as services were added.
	svcs, err := d.svcInformer.Lister().List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list services: %v", err)
	}
	for _, svc := range svcs {
		d.OnAdd(svc, false)
	}
	return nil
}

func (d *Discovery) OnAdd(obj interface{}, _ bool) {
	switch obj := obj.(type) {
	case *v1.Service:
		name := fmt.Sprintf("%s/%s", obj.Namespace, obj.Name)
		var match bool
		for _, selector := range d.parsedServiceSelectors {
			if selector.Matches(labels.Set(obj.Labels)) {
				match = true
			}
		}
		if !match {
			return
		}
		d.mutex.Lock()
		if _, ok := d.probes[name]; !ok {
			d.probes[name] = make(map[string]OnTargetRemove)
		}
		for _, ip := range obj.Spec.ClusterIPs {
			for _, port := range obj.Spec.Ports {
				target := fmt.Sprintf("%s:%d", ip, port.Port)
				if _, ok := d.probes[name][target]; !ok {
					d.probes[name][fmt.Sprintf("ClusterIP:%s", target)] = d.OnTargetAdd(target)
				}
			}
		}
		for _, port := range obj.Spec.Ports {
			if port.NodePort == 0 {
				continue
			}
			target := fmt.Sprintf("%s:%d", d.NodeIP, port.NodePort)
			if _, ok := d.probes[name][target]; !ok {
				d.probes[name][fmt.Sprintf("NodePort:%s", target)] = d.OnTargetAdd(target)
			}
		}
		d.mutex.Unlock()
		endpoints, err := d.endpointsInformer.Lister().Endpoints(obj.Namespace).Get(obj.Name)
		if err != nil {
			log.Err(err).Msg("failed to list endpoints")
			return
		}
		d.OnAdd(endpoints, false)
	case *v1.Endpoints:
		d.mutex.Lock()
		defer d.mutex.Unlock()
		name := fmt.Sprintf("%s/%s", obj.Namespace, obj.Name)
		if _, ok := d.probes[name]; !ok {
			// This service is not in the select set.
			return
		}
		for _, es := range obj.Subsets {
			for _, addr := range es.Addresses {
				for _, port := range es.Ports {
					target := fmt.Sprintf("%s:%d", addr.IP, port.Port)
					if _, ok := d.probes[name][target]; !ok {
						d.probes[name][target] = d.OnTargetAdd(target)
					}
				}
			}
		}
	default:
		log.Warn().Type("informer_type", obj).Msg("unexpected object type")
	}
}

func (d *Discovery) OnUpdate(_, obj interface{}) {
	switch obj := obj.(type) {
	case *v1.Service:
		var match bool
		for _, s := range d.parsedServiceSelectors {
			if s.Matches(labels.Set(obj.Labels)) {
				match = true
				return
			}
		}
		if match {
			d.OnAdd(obj, false)
		} else {
			d.OnDelete(obj)
		}
	case *v1.Endpoints:
		name := fmt.Sprintf("%s/%s", obj.Namespace, obj.Name)
		newTargets := make(map[string]struct{})
		for _, es := range obj.Subsets {
			for _, addr := range es.Addresses {
				for _, port := range es.Ports {
					target := fmt.Sprintf("%s:%d", addr.IP, port.Port)
					newTargets[target] = struct{}{}
				}
			}
		}
		d.mutex.Lock()
		defer d.mutex.Unlock()
		for target := range newTargets {
			if _, ok := d.probes[name][target]; !ok {
				d.probes[name][target] = d.OnTargetAdd(target)
			}
		}
		for target := range d.probes[name] { // remove stale targets
			if _, ok := newTargets[target]; !ok {
				if strings.HasPrefix(target, "ClusterIP:") || strings.HasPrefix(target, "NodePort:") {
					continue
				}
				// Need to skip svc targets
				d.probes[name][target]()
				delete(d.probes[name], target)
			}
		}
	default:
		log.Warn().Type("informer_type", obj).Msg("unexpected object type")
	}
}

func (d *Discovery) OnDelete(obj interface{}) {
	switch obj := obj.(type) {
	case *v1.Service:
		name := fmt.Sprintf("%s/%s", obj.Namespace, obj.Name)
		d.mutex.Lock()
		defer d.mutex.Unlock()
		probes, ok := d.probes[name]
		if !ok {
			return
		}
		for target, stop := range probes {
			log.Info().Str("target", target).Str("svc", name).Msg("removing target")
			stop()
			delete(probes, target)
		}
		delete(d.probes, name)
	case *v1.Endpoints:
		// Fake an empty update
		d.OnUpdate(obj, &v1.Endpoints{
			ObjectMeta: obj.ObjectMeta,
		})

	default:
		log.Warn().Type("informer_type", obj).Msg("unexpected object type")
		return
	}

}
