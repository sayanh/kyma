package testing

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	. "github.com/onsi/gomega"

	"golang.org/x/oauth2"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kyma-project/kyma/components/eventing-controller/pkg/ems2/api/events/config"
	bebtypes "github.com/kyma-project/kyma/components/eventing-controller/pkg/ems2/api/events/types"

	// gcp auth etc.
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

const (
	UrlAuth         = "/auth"
	UrlMessagingApi = "/messaging"
)

// BebMock implements a mock for BEB
type BebMock struct {
	Requests  []http.Request
	BebConfig *config.Config
}

func (m *BebMock) Start() string {
	// implementation based on https://pages.github.tools.sap/KernelServices/APIDefinitions/?urls.primaryName=Business%20Event%20Bus%20-%20CloudEvents
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := logf.Log.WithName("beb mock")
		payload, err := ioutil.ReadAll(r.Body)
		Expect(err).ShouldNot(HaveOccurred())

		log.V(1).Info("received request",
			"uri", r.RequestURI,
			"method", r.Method,
			"payload", payload,
		)
		config.GetDefaultConfig()

		m.Requests = append(m.Requests, *r)

		w.Header().Set("Content-Type", "application/json")

		// oauth2 request
		if r.Method == "POST" && r.RequestURI == UrlAuth {
			bebAuthResponseSuccess(w)
			return
		}
		// messaging API request
		if strings.HasPrefix(r.RequestURI, UrlMessagingApi) {
			switch r.Method {
			case http.MethodDelete:
				bebDeleteResponseSuccess(w)
			case http.MethodPost:
				bebCreateSuccess(w)
			case http.MethodGet:
				switch r.RequestURI {
				case m.BebConfig.ListURL:
					bebListSuccess(w)
				// get on a single subscription
				default:
					parsedUrl, err := url.Parse(r.RequestURI)
					Expect(err).ShouldNot(HaveOccurred())
					subscriptionName := parsedUrl.Path
					bebGetSuccess(w, subscriptionName)
				}
				return
			default:
				w.WriteHeader(http.StatusNotImplemented)
			}
			return
		}
	}))
	uri := ts.URL

	return uri
}

func bebAuthResponseSuccess(w http.ResponseWriter) {
	token := oauth2.Token{
		AccessToken:  "some-token",
		TokenType:    "",
		RefreshToken: "",
	}
	w.WriteHeader(http.StatusOK)
	err := json.NewEncoder(w).Encode(token)
	Expect(err).ShouldNot(HaveOccurred())
}

func bebCreateSuccess(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
	response := bebtypes.CreateResponse{
		Response: bebtypes.Response{
			StatusCode: http.StatusAccepted,
			Message:    "",
		},
		Href: "",
	}
	err := json.NewEncoder(w).Encode(response)
	Expect(err).ShouldNot(HaveOccurred())
}

func bebGetSuccess(w http.ResponseWriter, name string) {
	w.WriteHeader(http.StatusOK)
	s := bebtypes.Subscription{
		Name:               name,
		SubscriptionStatus: bebtypes.SubscriptionStatusActive,
	}
	err := json.NewEncoder(w).Encode(s)
	Expect(err).ShouldNot(HaveOccurred())
}

func bebListSuccess(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
	response := bebtypes.Response{
		StatusCode: http.StatusOK,
		Message:    "",
	}
	err := json.NewEncoder(w).Encode(response)
	Expect(err).ShouldNot(HaveOccurred())
}

func bebDeleteResponseSuccess(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}