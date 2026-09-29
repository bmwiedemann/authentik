package ldap

import (
	"fmt"
	"net"
	"time"

	"beryju.io/ldap"
	"github.com/getsentry/sentry-go"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"goauthentik.io/internal/outpost/ldap/bind"
	"goauthentik.io/internal/outpost/ldap/metrics"
)

func (ls *LDAPServer) Bind(r ldap.BindRequest, conn net.Conn) (code ldap.LDAPResultCode, err error) {
	req, span := bind.NewRequest(r, conn)
	selectedApp := ""
	defer func() {
		span.Finish()
		metrics.Requests.With(prometheus.Labels{
			"outpost_name": ls.ac.Outpost.Name,
			"type":         "bind",
			"app":          selectedApp,
		}).Observe(float64(span.EndTime.Sub(span.StartTime)) / float64(time.Second))
		req.Log().WithField("took-ms", span.EndTime.Sub(span.StartTime).Milliseconds()).Info("Bind request")
	}()

	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		recErr, ok := rec.(error)
		if !ok {
			recErr = fmt.Errorf("%v", rec)
		}
		log.WithError(recErr).Error("recover in bind request")
		sentry.CaptureException(recErr)
		// Without this a recovered panic returns the zero result code, which
		// is LDAPResultSuccess, and the bind is accepted without any check.
		code = ldap.LDAPResultOperationsError
		err = recErr
	}()

	for _, instance := range ls.providers {
		username, err := instance.binder.GetUsername(r.BindDN)
		if err == nil {
			selectedApp = instance.GetAppSlug()
			c, err := instance.binder.Bind(username, req)
			if c == ldap.LDAPResultSuccess {
				f := instance.GetFlags(req.BindDN)
				ls.connectionsSync.Lock()
				ls.connections[f.SessionID()] = conn
				ls.connectionsSync.Unlock()
			}
			return c, err
		} else {
			req.Log().WithError(err).Debug("Username not for instance")
		}
	}
	req.Log().WithField("request", "bind").Warning("No provider found for request")
	metrics.RequestsRejected.With(prometheus.Labels{
		"outpost_name": ls.ac.Outpost.Name,
		"type":         "bind",
		"reason":       "no_provider",
		"app":          "",
	}).Inc()

	return ldap.LDAPResultInsufficientAccessRights, nil
}
