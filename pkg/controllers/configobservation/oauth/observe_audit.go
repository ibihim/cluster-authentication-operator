package oauth

import (
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-authentication-operator/pkg/controllers/configobservation"
)

var (
	AuditOptionsPath = []string{
		"serverArguments",
	}
	auditOptionsArgs = map[string]interface{}{
		"audit-log-path":      "/var/log/oauth-server/audit.log",
		"audit-log-format":    "json",
		"audit-log-maxsize":   "100",
		"audit-log-maxbackup": "10",
		"audit-policy-file":   "/var/run/configmaps/audit/audit.yaml",
	}
)

func ObserveAudit(
	genericListers configobserver.Listers,
	recorder events.Recorder,
	existingConfig map[string]interface{},
) (ret map[string]interface{}, _ []error) {
	defer func() {
		ret = configobserver.Pruned(ret, AuditOptionsPath)
	}()

	listers := genericListers.(configobservation.Listers)
	var errs []error

	oauthConfig, err := listers.OAuthLister().Get("cluster")
	if errors.IsNotFound(err) {
		klog.Warning("oauth.config.openshift.io/cluster: not found")
		oauthConfig = new(configv1.OAuth)
		klog.V(2).Info("xxx there is no oauth")
	} else if err != nil {
		klog.V(2).Info("xxx err on get oauth")
		return existingConfig, []error{fmt.Errorf(
			"xxx get oauth.config.openshift.io/cluster: %w",
			err,
		)}
	}

	observedConfig := map[string]interface{}{}
	observedAuditProfile := oauthConfig.Spec.Audit.Profile
	if observedAuditProfile != configv1.OAuthNoneAuditProfileType {
		if err := unstructured.SetNestedField(
			observedConfig,
			auditOptionsArgs,
			AuditOptionsPath...,
		); err != nil {
			klog.V(2).Info("xxx err on adding nested fields")
			return existingConfig, append(errs, fmt.Errorf(
				"xxx set nested field (%s) for profile (%s): %w",
				strings.Join(AuditOptionsPath, "/"),
				observedAuditProfile,
				err,
			))
		}
	}

	currentAuditProfile, _, err := unstructured.NestedFieldCopy(
		existingConfig,
		AuditOptionsPath...,
	)
	if err != nil {
		klog.V(2).Info("xxx err on getting currrent stuff")
		return existingConfig, append(errs, err)
	}

	if !equality.Semantic.DeepEqual(currentAuditProfile, auditOptionsArgs) {
		recorder.Eventf(
			"ObserveAuditProfile",
			"AuditProfile changed from '%s' to '%s'",
			currentAuditProfile,
			auditOptionsArgs,
		)
		klog.V(2).Info("xxx state change")
	}

	klog.V(2).Infof("xxx observedConfig: %+v", observedConfig)

	return observedConfig, errs
}
