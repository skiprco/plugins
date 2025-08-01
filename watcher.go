package kubernetes

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"go-micro.dev/v4/logger"
	"go-micro.dev/v4/registry"

	"github.com/skiprco/go-micro-kubernetes-registry/client"
	"github.com/skiprco/go-micro-kubernetes-registry/client/watch"
)

var (
	deleteAction = "delete"
)

type k8sWatcher struct {
	registry *kregistry
	watcher  watch.Watch
	next     chan *registry.Result

	sync.RWMutex
	pods map[string]*client.Pod
	sync.Once
}

// build a cache of pods when the watcher starts.
func (k *k8sWatcher) updateCache() ([]*registry.Result, error) {
	podList, err := k.registry.client.ListPods(podSelector)
	if err != nil {
		return nil, err
	}

	var results []*registry.Result

	for _, p := range podList.Items {
		// Copy to new var as p gets overwritten by the loop
		pod := p
		rslts := k.buildPodResults(&pod, nil)
		results = append(results, rslts...)

		k.Lock()
		k.pods[pod.Metadata.Name] = &pod
		k.Unlock()
	}

	return results, nil
}

// look through pod annotations, compare against cache if present
// and return a list of results to send down the wire.
func (k *k8sWatcher) buildPodResults(pod *client.Pod, cache *client.Pod) []*registry.Result {
	var results []*registry.Result

	ignore := make(map[string]bool)

	if pod.Metadata != nil {
		results, ignore = podBuildResult(pod, cache)
	}

	// loop through cache annotations to find services
	// not accounted for above, and "delete" them.
	if cache != nil && cache.Metadata != nil {
		for annKey, annVal := range cache.Metadata.Annotations {
			if ignore[annKey] {
				continue
			}

			// check this annotation kv is a service notation
			if !strings.HasPrefix(annKey, annotationServiceKeyPrefix) {
				continue
			}

			rslt := &registry.Result{Action: deleteAction}

			// unmarshal service notation from annotation value
			if err := json.Unmarshal([]byte(*annVal), &rslt.Service); err != nil {
				continue
			}

			results = append(results, rslt)
		}
	}

	return results
}

// handleEvent will taken an event from the k8s pods API and do the correct
// things with the result, based on the local cache.
func (k *k8sWatcher) handleEvent(event watch.Event) {
	var pod client.Pod
	if err := json.Unmarshal([]byte(event.Object), &pod); err != nil {
		logger.Error("K8s Watcher: Couldnt unmarshal event object from pod")
		return
	}

	//nolint:exhaustive
	switch event.Type {
	// Pod was modified
	case watch.Modified:
		k.RLock()
		cache := k.pods[pod.Metadata.Name]
		k.RUnlock()

		// service could have been added, edited or removed.
		var results []*registry.Result

		if pod.Status.Phase == podRunning {
			results = k.buildPodResults(&pod, cache)
		} else {
			// passing in cache might not return all results
			results = k.buildPodResults(&pod, nil)
		}

		for _, result := range results {
			// pod isnt running
			if pod.Status.Phase != podRunning || pod.Metadata.DeletionTimestamp != "" {
				result.Action = deleteAction
			}
			k.next <- result
		}

		k.Lock()
		k.pods[pod.Metadata.Name] = &pod
		k.Unlock()

		return

	// Pod was deleted
	// passing in cache might not return all results
	case watch.Deleted:
		results := k.buildPodResults(&pod, nil)

		for _, result := range results {
			result.Action = deleteAction
			k.next <- result
		}

		k.Lock()
		delete(k.pods, pod.Metadata.Name)
		k.Unlock()

		return
	}
}

// Next will block until a new result comes in.
func (k *k8sWatcher) Next() (*registry.Result, error) {
	r, ok := <-k.next
	if !ok {
		return nil, errors.New("result chan closed")
	}

	return r, nil
}

// Stop will cancel any requests, and close channels.
func (k *k8sWatcher) Stop() {
	k.watcher.Stop()

	select {
	case <-k.next:
		return
	default:
		k.Do(func() {
			close(k.next)
		})
	}
}

func newWatcher(kr *kregistry, opts ...registry.WatchOption) (registry.Watcher, error) {
	var wo registry.WatchOptions
	for _, o := range opts {
		o(&wo)
	}

	selector := podSelector
	if len(wo.Service) > 0 {
		selector = map[string]string{
			svcSelectorPrefix + serviceName(wo.Service): svcSelectorValue,
		}
	}

	// Create watch request
	watcher, err := kr.client.WatchPods(selector)
	if err != nil {
		return nil, err
	}

	k := &k8sWatcher{
		registry: kr,
		watcher:  watcher,
		next:     make(chan *registry.Result),
		pods:     make(map[string]*client.Pod),
	}

	// update cache, but dont emit changes
	if _, err := k.updateCache(); err != nil {
		return nil, err
	}

	// range over watch request changes, and invoke
	// the update event
	go func() {
		for event := range watcher.ResultChan() {
			k.handleEvent(event)
		}

		k.Stop()
	}()

	return k, nil
}

func podBuildResult(pod *client.Pod, cache *client.Pod) ([]*registry.Result, map[string]bool) {
	results := make([]*registry.Result, 0, len(pod.Metadata.Annotations))
	ignore := make(map[string]bool)

	for annKey, annVal := range pod.Metadata.Annotations {
		// check this annotation kv is a service notation
		if !strings.HasPrefix(annKey, annotationServiceKeyPrefix) {
			continue
		}

		if annVal == nil {
			continue
		}

		// ignore when we check the cached annotations
		// as we take care of it here
		ignore[annKey] = true

		// compare against cache.
		var (
			cacheExists bool
			cav         *string
		)

		if cache != nil && cache.Metadata != nil {
			cav, cacheExists = cache.Metadata.Annotations[annKey]
			if cacheExists && cav != nil && cav == annVal {
				// service notation exists and is identical -
				// no change result required.
				continue
			}
		}

		rslt := &registry.Result{}
		if cacheExists {
			rslt.Action = "update"
		} else {
			rslt.Action = "create"
		}

		// unmarshal service notation from annotation value
		if err := json.Unmarshal([]byte(*annVal), &rslt.Service); err != nil {
			continue
		}

		results = append(results, rslt)
	}

	return results, ignore
}
