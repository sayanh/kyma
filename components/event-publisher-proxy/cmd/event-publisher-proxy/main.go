package main

import (
	"github.com/kelseyhightower/envconfig"
	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/legacy-events"
	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/options"
	"github.com/sirupsen/logrus"

	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/env"
	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/handler"
	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/oauth"
	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/receiver"
	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/sender"
	"github.com/kyma-project/kyma/components/event-publisher-proxy/pkg/signals"
)

func main() {
	logger := logrus.New()
	opts := options.ParseArgs()
	cfg := new(env.Config)
	if err := envconfig.Process("", cfg); err != nil {
		logger.Fatalf("Start handler failed with error: %s", err)
	}

	logger.Info("Start the Event Publisher Proxy")

	// configure message receiver
	messageReceiver := receiver.NewHttpMessageReceiver(cfg.Port)

	// configure auth client
	ctx := signals.NewContext()
	client := oauth.NewClient(ctx, cfg)
	defer client.CloseIdleConnections()

	// configure message sender
	messageSender := sender.NewHttpMessageSender(cfg.EmsPublishURL, client)

	// configure legacyTransformer
	legacyTransformer := legacy.NewTransformer(
		cfg.BEBNamespace,
		cfg.EventTypePrefix,
	)
	// start handler which blocks until it receives a shutdown signal
	if err := handler.NewHandler(messageReceiver, messageSender, cfg.RequestTimeout, legacyTransformer, opts, logger).Start(ctx); err != nil {
		logger.Fatalf("Start handler failed with error: %s", err)
	}

	logger.Info("Shutdown the Event Publisher Proxy")
}
