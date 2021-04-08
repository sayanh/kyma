package eventing

import (
	"fmt"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/kyma-project/kyma/tests/end-to-end/upgrade/pkg/tests/eventing/helpers"
)

const (
	integrationNamespace    = "kyma-integration"
	eventServiceSuffix      = "event-service"
	eventServicePort        = "8081"
	defaultName             = "eventupgrade"
	defaultEventType        = "order.created"
	defaultEventTypeVersion = "v1"
	defaultBrokerName       = "default"
)

type eventMeshFlow struct {
	EventMeshUpgradeTest
	namespace string

	applicationName     string
	serviceInstanceName string
	subscriberName      string
	subscriptionName    string
	eventTypeVersion    string
	eventType           string
	brokerName          string

	log  logrus.FieldLogger
	stop <-chan struct{}
}

func newEventMeshFlow(e *EventMeshUpgradeTest,
	stop <-chan struct{}, log logrus.FieldLogger, namespace string) *eventMeshFlow {
	return &eventMeshFlow{
		EventMeshUpgradeTest: *e,
		stop:                 stop,
		log:                  log,
		namespace:            namespace,
		applicationName:      defaultName,
		serviceInstanceName:  defaultName,
		subscriberName:       defaultName,
		eventTypeVersion:     defaultEventTypeVersion,
		eventType:            defaultEventType,
		subscriptionName:     defaultName,
		brokerName:           defaultBrokerName,
	}
}

func (f *eventMeshFlow) CreateApplication() error {
	return helpers.CreateApplication(f.appConnectorInterface, f.applicationName,
		helpers.WithAccessLabel(f.applicationName),
		helpers.WithEventService(f.serviceInstanceName),
	)
}

func (f *eventMeshFlow) CreateSubscriber() error {
	return helpers.CreateSubscriber(f.k8sInterface, f.subscriberName, f.namespace, helpers.WithSubscriberImage(f.subscriberImage))
}

func (f *eventMeshFlow) WaitForSubscriber() error {
	return helpers.WaitForSubscriber(f.k8sInterface, f.subscriberName, f.namespace)
}

func (f *eventMeshFlow) CreateApplicationMapping() error {
	return helpers.CreateApplicationMapping(f.appBrokerCli, f.applicationName, f.namespace)
}

func (f *eventMeshFlow) CreateServiceInstance() error {
	return helpers.CreateServiceInstance(f.scCli, f.serviceInstanceName, f.namespace)
}

func (f *eventMeshFlow) CheckEvent() error {
	return helpers.CheckEvent(fmt.Sprintf("http://%s.%s.svc.cluster.local:9000/ce/%v/%v/%v", f.subscriberName, f.namespace, f.applicationName, f.eventType, f.eventTypeVersion), http.StatusOK)
}

func (f *eventMeshFlow) WaitForServiceInstance() error {
	return helpers.WaitForServiceInstance(f.scCli, f.serviceInstanceName, f.namespace)
}

func (f *eventMeshFlow) PublishTestEvent() error {
	return helpers.SendEvent(fmt.Sprintf("http://%s-%s.%s.svc.cluster.local:%s/%s/v1/events", f.applicationName, eventServiceSuffix, integrationNamespace, eventServicePort, f.applicationName), f.eventType, f.eventTypeVersion)
}
