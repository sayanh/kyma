package compass

import (
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"github.com/vrischmann/envconfig"

	"github.com/kyma-project/kyma/tests/end-to-end/external-solution-integration/internal/scenario"
)

// Scenario executes complete external solution integration test scenario
// using Compass for Application registration and connectivity
type Scenario struct {
	domain         string
	testID         string
	skipSSLVerify  bool
	eventSendDelay time.Duration
}

// AddFlags adds CLI flags to given FlagSet
func (s *Scenario) AddFlags(set *pflag.FlagSet) {
	pflag.StringVar(&s.domain, "domain", "kyma.local", "domain")
	pflag.StringVar(&s.testID, "testID", "compass-e2e-test", "domain")
	pflag.BoolVar(&s.skipSSLVerify, "skipSSLVerify", false, "Skip verification of service SSL certificates")
	pflag.DurationVar(&s.eventSendDelay, "eventSendDelay", time.Duration(10)*time.Second, "Wait time in seconds before sending the first event, e.g. 5s")
}

func (s *Scenario) NewState() (*state, error) {
	config := scenario.CompassEnvConfig{}
	err := envconfig.Init(&config)
	if err != nil {
		return nil, errors.Wrap(err, "while loading environment variables")
	}

	return &state{
		E2EState:         scenario.E2EState{Domain: s.domain, SkipSSLVerify: s.skipSSLVerify, AppName: s.testID, GatewaySubdomain: "gateway"},
		CompassEnvConfig: config,
	}, nil
}
