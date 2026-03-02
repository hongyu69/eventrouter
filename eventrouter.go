/*
Copyright 2017 Heptio Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/heptiolabs/eventrouter/sinks"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/viper"

	eventsv1 "k8s.io/api/events/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	eventsinformers "k8s.io/client-go/informers/events/v1"
	"k8s.io/client-go/kubernetes"
	eventslisters "k8s.io/client-go/listers/events/v1"
	"k8s.io/client-go/tools/cache"
)

var (
	kubernetesWarningEventCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "heptio_eventrouter_warnings_total",
		Help: "Total number of warning events in the kubernetes cluster",
	}, []string{
		"involved_object_kind",
		"involved_object_name",
		"involved_object_namespace",
		"reason",
		"source",
	})
	kubernetesNormalEventCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "heptio_eventrouter_normal_total",
		Help: "Total number of normal events in the kubernetes cluster",
	}, []string{
		"involved_object_kind",
		"involved_object_name",
		"involved_object_namespace",
		"reason",
		"source",
	})
	kubernetesInfoEventCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "heptio_eventrouter_info_total",
		Help: "Total number of info events in the kubernetes cluster",
	}, []string{
		"involved_object_kind",
		"involved_object_name",
		"involved_object_namespace",
		"reason",
		"source",
	})
	kubernetesUnknownEventCounterVec = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "heptio_eventrouter_unknown_total",
		Help: "Total number of events of unknown type in the kubernetes cluster",
	}, []string{
		"involved_object_kind",
		"involved_object_name",
		"involved_object_namespace",
		"reason",
		"source",
	})
)

// EventRouter is responsible for maintaining a stream of kubernetes
// system Events and pushing them to another channel for storage
type EventRouter struct {
	// kubeclient is the main kubernetes interface
	kubeClient *kubernetes.Clientset

	// store of events populated by the shared informer
	eLister eventslisters.EventLister

	// returns true if the event store has been synced
	eListerSynched cache.InformerSynced

	// event sink
	// TODO: Determine if we want to support multiple sinks.
	eSink sinks.EventSinkInterface

	// startTime records when the router started, used to filter events
	startTime time.Time
}

// NewEventRouter will create a new event router using the input params
func NewEventRouter(kubeClient *kubernetes.Clientset, eventsInformer eventsinformers.EventInformer) *EventRouter {
	if viper.GetBool("enable-prometheus") {
		prometheus.MustRegister(kubernetesWarningEventCounterVec)
		prometheus.MustRegister(kubernetesNormalEventCounterVec)
		prometheus.MustRegister(kubernetesInfoEventCounterVec)
		prometheus.MustRegister(kubernetesUnknownEventCounterVec)
	}

	er := &EventRouter{
		kubeClient: kubeClient,
		eSink:      sinks.ManufactureSink(),
		startTime:  time.Now().UTC(),
	}
	eventsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    er.addEvent,
		UpdateFunc: er.updateEvent,
		DeleteFunc: er.deleteEvent,
	})
	er.eLister = eventsInformer.Lister()
	er.eListerSynched = eventsInformer.Informer().HasSynced
	return er
}

// Run starts the EventRouter/Controller.
func (er *EventRouter) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer glog.Infof("Shutting down EventRouter")

	glog.Infof("Starting EventRouter")

	// here is where we kick the caches into gear
	if !cache.WaitForCacheSync(stopCh, er.eListerSynched) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}
	<-stopCh
}

// addEvent is called when an event is created, or during the initial list
func (er *EventRouter) addEvent(obj interface{}) {
	e := obj.(*eventsv1.Event)
	if er.eventLastSeenAfterStart(e) {
		prometheusEvent(e)
		er.eSink.UpdateEvents(e, nil)
	} else {
		glog.V(3).Infof("Skipping pre-start event %s/%s last seen at %s (router start %s)", e.Namespace, e.Name, eventLastObservedTime(e), er.startTime)
	}
}

// updateEvent is called any time there is an update to an existing event
func (er *EventRouter) updateEvent(objOld interface{}, objNew interface{}) {
	eOld := objOld.(*eventsv1.Event)
	eNew := objNew.(*eventsv1.Event)
	if er.eventLastSeenAfterStart(eNew) {
		prometheusEvent(eNew)
		er.eSink.UpdateEvents(eNew, eOld)
	} else {
		glog.V(3).Infof("Skipping update for pre-start event %s/%s last seen at %s (router start %s)", eNew.Namespace, eNew.Name, eventLastObservedTime(eNew), er.startTime)
	}
}

// eventLastObservedTime returns the most recent observation time from the event.
// For series events, Series.LastObservedTime is used. Otherwise EventTime is preferred,
// falling back to deprecated fields.
func eventLastObservedTime(e *eventsv1.Event) time.Time {
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return e.Series.LastObservedTime.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.DeprecatedLastTimestamp.IsZero() {
		return e.DeprecatedLastTimestamp.Time
	}
	if !e.DeprecatedFirstTimestamp.IsZero() {
		return e.DeprecatedFirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// eventLastSeenAfterStart determines if an event should be published by
// checking its occurrence time against the router start time.
// Preference order: Series.LastObservedTime, EventTime, DeprecatedLastTimestamp,
// DeprecatedFirstTimestamp, CreationTimestamp.
// If none are available, the event is dropped.
func (er *EventRouter) eventLastSeenAfterStart(e *eventsv1.Event) bool {
	t := eventLastObservedTime(e)
	if t.IsZero() {
		return false
	}
	return !t.UTC().Before(er.startTime)
}

// prometheusEvent is called when an event is added or updated
func prometheusEvent(event *eventsv1.Event) {
	if !viper.GetBool("enable-prometheus") {
		return
	}
	var counter prometheus.Counter
	var err error

	switch event.Type {
	case "Normal":
		counter, err = kubernetesNormalEventCounterVec.GetMetricWithLabelValues(
			event.Regarding.Kind,
			event.Regarding.Name,
			event.Regarding.Namespace,
			event.Reason,
			event.ReportingInstance,
		)
	case "Warning":
		counter, err = kubernetesWarningEventCounterVec.GetMetricWithLabelValues(
			event.Regarding.Kind,
			event.Regarding.Name,
			event.Regarding.Namespace,
			event.Reason,
			event.ReportingInstance,
		)
	case "Info":
		counter, err = kubernetesInfoEventCounterVec.GetMetricWithLabelValues(
			event.Regarding.Kind,
			event.Regarding.Name,
			event.Regarding.Namespace,
			event.Reason,
			event.ReportingInstance,
		)
	default:
		counter, err = kubernetesUnknownEventCounterVec.GetMetricWithLabelValues(
			event.Regarding.Kind,
			event.Regarding.Name,
			event.Regarding.Namespace,
			event.Reason,
			event.ReportingInstance,
		)
	}

	if err != nil {
		// Not sure this is the right place to log this error?
		glog.Warning(err)
	} else {
		counter.Add(1)
	}
}

// deleteEvent should only occur when the system garbage collects events via TTL expiration
func (er *EventRouter) deleteEvent(obj interface{}) {
	e := obj.(*eventsv1.Event)
	// NOTE: This should *only* happen on TTL expiration there
	// is no reason to push this to a sink
	glog.V(5).Infof("Event Deleted from the system:\n%v", e)
}
