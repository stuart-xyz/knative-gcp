/*
Copyright 2020 Google LLC

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

package ingress

import (
	"context"
	"fmt"
	"os"
	"sync"

	"cloud.google.com/go/pubsub"
	"go.opencensus.io/trace"
	"go.uber.org/zap"

	cepubsub "github.com/cloudevents/sdk-go/protocol/pubsub/v2"
	cev2 "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/cloudevents/sdk-go/v2/extensions"
	"github.com/cloudevents/sdk-go/v2/protocol"
	"github.com/google/knative-gcp/pkg/broker/config"
	"github.com/google/knative-gcp/pkg/broker/handler/processors/filter"
	"github.com/google/knative-gcp/pkg/logging"
)

const projectEnvKey = "PROJECT_ID"

// NewMultiTopicDecoupleSink creates a new multiTopicDecoupleSink.
func NewMultiTopicDecoupleSink(
	ctx context.Context,
	brokerConfig config.ReadonlyTargets,
	client *pubsub.Client,
	publishSettings pubsub.PublishSettings) *multiTopicDecoupleSink {

	return &multiTopicDecoupleSink{
		pubsub:          client,
		publishSettings: publishSettings,
		brokerConfig:    brokerConfig,
		// TODO(#1118): remove Topic when broker config is removed
		topics: make(map[config.BrokerKey]*pubsub.Topic),
		// TODO(#1804): remove this field when enabling the feature by default.
		enableEventFiltering: enableEventFilterFunc(),
	}
}

// multiTopicDecoupleSink implements DecoupleSink and routes events to pubsub topics corresponding
// to the broker to which the events are sent.
type multiTopicDecoupleSink struct {
	// pubsub talks to pubsub.
	pubsub          *pubsub.Client
	publishSettings pubsub.PublishSettings
	// map from brokers to topics
	topics    map[config.BrokerKey]*pubsub.Topic
	topicsMut sync.RWMutex
	// brokerConfig holds configurations for all brokers. It's a view of a configmap populated by
	// the broker controller.
	brokerConfig config.ReadonlyTargets
	// TODO(#1804): remove this field when enabling the feature by default.
	enableEventFiltering bool
}

// Send sends incoming event to its corresponding pubsub topic based on which broker it belongs to.
func (m *multiTopicDecoupleSink) Send(ctx context.Context, broker *config.BrokerKey, event cev2.Event) protocol.Result {
	topic, err := m.getTopicForBroker(ctx, broker)
	if err != nil {
		trace.FromContext(ctx).Annotate(
			[]trace.Attribute{
				trace.StringAttribute("error_message", err.Error()),
			},
			"unable to accept event",
		)
		return err
	}

	// Check to see if there are any triggers interested in this event. If not, no need to send this
	// to the decouple topic.
	// TODO(#1804): remove first check when enabling the feature by default.
	if m.enableEventFiltering && !m.hasTrigger(ctx, &event) {
		logging.FromContext(ctx).Debug("Filering target-less event at ingress", zap.String("Eventid", event.ID()))
		return nil
	}

	dt := extensions.FromSpanContext(trace.FromContext(ctx).SpanContext())
	msg := new(pubsub.Message)
	if err := cepubsub.WritePubSubMessage(ctx, binding.ToMessage(&event), msg, dt.WriteTransformer()); err != nil {
		return err
	}

	_, err = topic.Publish(ctx, msg).Get(ctx)
	return err
}

// eventFilterFunc is used to see if a target is interested in an event.
// It is used as a vaiable to allow stubbing out in unit tests.
var eventFilterFunc = filter.PassFilter

// enableEventFilterFunc is a temporary function to control enabling and
// disabling trigger-less event filtering in ingress.
// TODO(#1804): remove this variable when enabling the feature by default.
var enableEventFilterFunc = isEventFilteringEnabled

// TODO(#1804): remove this method when enabling the feature by default.
func isEventFilteringEnabled() bool {
	return os.Getenv("ENABLE_INGRESS_EVENT_FILTERING") == "true"
}

// hasTrigger checks given event against all targets to see if it will pass any of their filters.
// If one is fouund, hasTrigger returns true.
func (m *multiTopicDecoupleSink) hasTrigger(ctx context.Context, event *cev2.Event) bool {
	hasTrigger := false
	m.brokerConfig.RangeAllTargets(func(target *config.Target) bool {
		if eventFilterFunc(ctx, target.FilterAttributes, event) {
			hasTrigger = true
			return false
		}

		return true
	})

	return hasTrigger
}

// getTopicForBroker finds the corresponding decouple topic for the broker from the mounted broker configmap volume.
func (m *multiTopicDecoupleSink) getTopicForBroker(ctx context.Context, broker *config.BrokerKey) (*pubsub.Topic, error) {
	topicID, err := m.getTopicIDForBroker(ctx, broker)
	if err != nil {
		return nil, err
	}

	if topic, ok := m.getExistingTopic(broker); ok {
		// Check that the broker's topic ID hasn't changed.
		if topic.ID() == topicID {
			return topic, nil
		}
	}

	// Topic needs to be created or updated.
	return m.updateTopicForBroker(ctx, broker)
}

func (m *multiTopicDecoupleSink) updateTopicForBroker(ctx context.Context, broker *config.BrokerKey) (*pubsub.Topic, error) {
	m.topicsMut.Lock()
	defer m.topicsMut.Unlock()
	// Fetch latest decouple topic ID under lock.
	topicID, err := m.getTopicIDForBroker(ctx, broker)
	if err != nil {
		return nil, err
	}

	if topic, ok := m.topics[*broker]; ok {
		if topic.ID() == topicID {
			// Topic already updated.
			return topic, nil
		}
		// Stop old topic.
		m.topics[*broker].Stop()
	}
	topic := m.pubsub.Topic(topicID)
	m.topics[*broker] = topic
	return topic, nil
}

func (m *multiTopicDecoupleSink) getTopicIDForBroker(ctx context.Context, broker *config.BrokerKey) (string, error) {
	brokerConfig, ok := m.brokerConfig.GetBrokerByKey(broker)
	if !ok {
		// There is an propagation delay between the controller reconciles the broker config and
		// the config being pushed to the configmap volume in the ingress pod. So sometimes we return
		// an error even if the request is valid.
		logging.FromContext(ctx).Warn("config is not found for")
		return "", fmt.Errorf("%q: %w", broker, ErrNotFound)
	}
	if brokerConfig.DecoupleQueue == nil || brokerConfig.DecoupleQueue.Topic == "" {
		logging.FromContext(ctx).Error("DecoupleQueue or topic missing for broker, this should NOT happen.", zap.Any("brokerConfig", brokerConfig))
		return "", fmt.Errorf("decouple queue of %q: %w", broker, ErrIncomplete)
	}
	if brokerConfig.DecoupleQueue.State != config.State_READY {
		logging.FromContext(ctx).Debug("decouple queue is not ready")
		return "", fmt.Errorf("%q: %w", broker, ErrNotReady)
	}
	return brokerConfig.DecoupleQueue.Topic, nil
}

func (m *multiTopicDecoupleSink) getExistingTopic(broker *config.BrokerKey) (*pubsub.Topic, bool) {
	m.topicsMut.RLock()
	defer m.topicsMut.RUnlock()
	topic, ok := m.topics[*broker]
	return topic, ok
}
