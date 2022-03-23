package deployment

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/ghodss/yaml"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"

	"github.com/openshift/cluster-authentication-operator/pkg/controllers/common"
	"github.com/openshift/cluster-authentication-operator/pkg/controllers/configobservation"
	observeoauth "github.com/openshift/cluster-authentication-operator/pkg/controllers/configobservation/oauth"
	"github.com/openshift/cluster-authentication-operator/pkg/operator/assets"
	"github.com/openshift/cluster-authentication-operator/pkg/operator/datasync"
)

func getOAuthServerDeployment(
	operatorConfig *operatorv1.Authentication,
	proxyConfig *configv1.Proxy,
	bootstrapUserExists bool,
	resourceVersions ...string,
) (*appsv1.Deployment, error) {
	// load deployment
	deployment := resourceread.ReadDeploymentV1OrDie(assets.MustAsset("oauth-openshift/deployment.yaml"))

	// force redeploy when any associated resource changes
	// we use a hash to prevent this value from growing indefinitely
	// need to sort first in order to get a stable array
	sort.Strings(resourceVersions)
	rvs := strings.Join(resourceVersions, ",")
	klog.V(4).Infof("tracked resource versions: %s", rvs)
	rvsHash := sha512.Sum512([]byte(rvs))
	rvsHashStr := base64.RawURLEncoding.EncodeToString(rvsHash[:])
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	deployment.Annotations["operator.openshift.io/rvs-hash"] = rvsHashStr

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations["operator.openshift.io/rvs-hash"] = rvsHashStr

	// Ensure a rollout when the bootstrap user goes away
	if bootstrapUserExists {
		deployment.Spec.Template.Annotations["operator.openshift.io/bootstrap-user-exists"] = "true"
	}

	templateSpec := &deployment.Spec.Template.Spec
	container := &templateSpec.Containers[0]

	// image spec
	if container.Image == "${IMAGE}" {
		container.Image = os.Getenv("IMAGE_OAUTH_SERVER")
	}

	// set proxy env vars
	container.Env = append(container.Env, proxyConfigToEnvVars(proxyConfig)...)

	// set log level
	container.Args[0] = strings.Replace(container.Args[0], "${LOG_LEVEL}", "5", -1) // fmt.Sprintf("%d", getLogLevel(operatorConfig.Spec.LogLevel)), -1)

	idpSyncData, err := getSyncDataFromOperatorConfig(&operatorConfig.Spec.ObservedConfig)
	if err != nil {
		return nil, fmt.Errorf("Unable to get IDP sync data: %v", err)
	}

	// mount more secrets and config maps
	v, m, err := idpSyncData.ToVolumesAndMounts()
	if err != nil {
		return nil, fmt.Errorf("Unable to transform observed IDP sync data to volumes and mounts: %v", err)
	}
	templateSpec.Volumes = append(templateSpec.Volumes, v...)
	container.VolumeMounts = append(container.VolumeMounts, m...)

	args, err := getServerArguments(&operatorConfig.Spec.ObservedConfig)
	if err != nil {
		return nil, fmt.Errorf("Unable to get audit sync data: %w", err)
	}

	flags := serverArgsToStringSlice(args)

	klog.Infof("xxx serverArguments: %s", flags)

	container.Args[0] = strings.Replace(
		container.Args[0],
		"${SERVER_ARGUMENTS}",
		stringSliceToNewLinedString(flags),
		1,
	)

	klog.Info("xxx deployment: %+v", deployment)

	return deployment, nil
}

type serverArguments map[string][]string

func getServerArguments(operatorConfig *runtime.RawExtension) (serverArguments, error) {
	var args serverArguments

	oauthServerObservedConfig, err := common.UnstructuredConfigFrom(
		operatorConfig.Raw,
		configobservation.OAuthServerConfigPrefix,
	)
	if err != nil {
		return args, fmt.Errorf("failed to grab the operator config: %w", err)
	}

	configDeserialized := new(struct {
		Args map[string]interface{} `json:"serverArguments"` // Now this thing is screwed.
	})
	if err := json.Unmarshal(oauthServerObservedConfig, &configDeserialized); err != nil {
		return args, fmt.Errorf("failed to unmarshal the observedConfig: %v", err)
	}

	klog.Infof("xxx configDeserialized: %+v", configDeserialized)

	for argName, argValue := range configDeserialized.Args {
		var argsSlice []string

		argsSlice, found, err := unstructured.NestedStringSlice(configDeserialized.Args, argName)
		if !found || err != nil {
			str, found, err := unstructured.NestedString(configDeserialized.Args, argName)
			if !found || err != nil {
				return nil, fmt.Errorf(
					"unable to create server arguments, incorrect value %v under %s key, expected []string or string",
					argValue, argName,
				)
			}

			argsSlice = append(argsSlice, str)
		}

		escapedArgsSlice := make([]string, 0, len(argsSlice))
		for i, str := range argsSlice {
			escapedArgsSlice[i] = maybeQuote(str)
		}

		args[argName] = escapedArgsSlice
	}

	return args, nil
}

func serverArgsToStringSlice(args serverArguments) []string {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var flags []string
	for _, key := range keys {
		for _, token := range args[key] {
			flags = append(flags, fmt.Sprintf("--%s=%v", key, token))
		}
	}

	return flags
}

func stringSliceToNewLinedString(flags []string) string {
	newLiner := times(
		fmt.Sprintf(" \\\n"),
		len(flags)-1,
	)

	var flagBlob strings.Builder

	for _, flag := range flags {
		flagBlob.WriteString(flag)
		flagBlob.WriteString(newLiner())
	}

	return flagBlob.String()
}

// times a generic candidate
func times(s string, t int) func() string {
	return func() string {
		if t > 0 {
			t = t - 1
			return s
		}

		return ""
	}
}

var (
	shellEscapePattern = regexp.MustCompile(`[^\w@%+=:,./-]`)
)

// maybeQuote returns a shell-escaped version of the string s. The returned value
// is a string that can safely be used as one token in a shell command line.
//
// note: this method was copied from https://github.com/alessio/shellescape/blob/0d13ae33b78a20a5d91c54ca7e216e1b75aaedef/shellescape.go#L30
func maybeQuote(s string) string {
	if len(s) == 0 {
		return "''"
	}
	if shellEscapePattern.MatchString(s) {
		return "'" + strings.Replace(s, "'", "'\"'\"'", -1) + "'"
	}

	return s
}

func getSyncDataFromOperatorConfig(operatorConfig *runtime.RawExtension) (*datasync.ConfigSyncData, error) {
	var configDeserialized map[string]interface{}
	oauthServerObservedConfig, err := common.UnstructuredConfigFrom(
		operatorConfig.Raw,
		configobservation.OAuthServerConfigPrefix,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to grab the operator config: %w", err)
	}

	if err := yaml.Unmarshal(oauthServerObservedConfig, &configDeserialized); err != nil {
		return nil, fmt.Errorf("failed to unmarshal the observedConfig: %v", err)
	}

	return observeoauth.GetIDPConfigSyncData(configDeserialized)
}

// TODO: reuse the library-go helper for this
func getLogLevel(logLevel operatorv1.LogLevel) int {
	switch logLevel {
	case operatorv1.Normal, "": // treat empty string to mean the default
		return 2
	case operatorv1.Debug:
		return 4
	case operatorv1.Trace:
		return 6
	case operatorv1.TraceAll:
		return 100 // this is supposed to be 8 but I prefer "all" to really mean all
	default:
		return 0
	}
}

// TODO: move to library-go:w
func proxyConfigToEnvVars(proxy *configv1.Proxy) []corev1.EnvVar {
	var envVars []corev1.EnvVar
	envVars = appendEnvVar(envVars, "NO_PROXY", proxy.Status.NoProxy)
	envVars = appendEnvVar(envVars, "HTTP_PROXY", proxy.Status.HTTPProxy)
	envVars = appendEnvVar(envVars, "HTTPS_PROXY", proxy.Status.HTTPSProxy)
	return envVars
}

func appendEnvVar(envVars []corev1.EnvVar, envName, envVal string) []corev1.EnvVar {
	if len(envVal) > 0 {
		return append(envVars, corev1.EnvVar{Name: envName, Value: envVal})
	}
	return envVars
}
