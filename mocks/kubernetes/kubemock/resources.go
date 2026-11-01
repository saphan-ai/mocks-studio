package kubemock

import (
	"encoding/json"
	"fmt"
	"regexp"
)

const (
	maxSubdomainLen = 253
	maxLabelLen     = 63
	generatedSuffix = 5
	defaultReplicas = 1
	suffixAlphabet  = "bcdfghjklmnpqrstvwxz2456789"
)

// resourceDef describes one resource of the slice.
type resourceDef struct {
	group      string // "" is the core group
	version    string
	plural     string
	singular   string
	kind       string
	namespaced bool
	validate   func(obj map[string]any) []statusCause
	defaults   func(obj map[string]any)
}

func (d *resourceDef) apiVersion() string {
	if d.group == "" {
		return d.version
	}
	return d.group + "/" + d.version
}

// qualified is how the API names the resource in messages: "pods", "deployments.apps".
func (d *resourceDef) qualified() string {
	if d.group == "" {
		return d.plural
	}
	return d.plural + "." + d.group
}

var (
	namespaceDef  = &resourceDef{version: "v1", plural: "namespaces", singular: "namespace", kind: "Namespace", validate: validateNamespace, defaults: defaultNamespace}
	podDef        = &resourceDef{version: "v1", plural: "pods", singular: "pod", kind: "Pod", namespaced: true, validate: validatePod, defaults: defaultPod}
	configMapDef  = &resourceDef{version: "v1", plural: "configmaps", singular: "configmap", kind: "ConfigMap", namespaced: true, validate: validateConfigMap}
	deploymentDef = &resourceDef{group: "apps", version: "v1", plural: "deployments", singular: "deployment", kind: "Deployment", namespaced: true, validate: validateDeployment, defaults: defaultDeployment}

	allResources = []*resourceDef{namespaceDef, podDef, configMapDef, deploymentDef}
)

var (
	subdomainPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	labelPattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	configKeyPattern = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

const (
	subdomainRule = "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character"
	labelRule     = "a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character"
)

func invalid(field, msg string) statusCause {
	return statusCause{Type: "FieldValueInvalid", Field: field, Message: msg}
}

func required(field string) statusCause {
	return statusCause{Type: "FieldValueRequired", Field: field, Message: "Required value"}
}

// validateName checks metadata.name against the rule the resource uses.
func validateName(name string, label bool) []statusCause {
	switch {
	case name == "":
		return []statusCause{required("metadata.name")}
	case label && (len(name) > maxLabelLen || !labelPattern.MatchString(name)):
		return []statusCause{invalid("metadata.name", fmt.Sprintf("Invalid value: %q: %s", name, labelRule))}
	case !label && (len(name) > maxSubdomainLen || !subdomainPattern.MatchString(name)):
		return []statusCause{invalid("metadata.name", fmt.Sprintf("Invalid value: %q: %s", name, subdomainRule))}
	}
	return nil
}

func validateNamespace(obj map[string]any) []statusCause {
	return validateName(str(path(obj, "metadata", "name")), true)
}

func validateConfigMap(obj map[string]any) []statusCause {
	causes := validateName(str(path(obj, "metadata", "name")), false)
	for _, block := range []string{"data", "binaryData"} {
		entries, _ := obj[block].(map[string]any)
		for k := range entries {
			if !configKeyPattern.MatchString(k) {
				causes = append(causes, invalid(fmt.Sprintf("%s[%s]", block, k), "a valid config key must consist of alphanumeric characters, '-', '_' or '.'"))
			}
		}
	}
	return causes
}

func validateContainers(spec map[string]any, prefix string) []statusCause {
	list, _ := spec["containers"].([]any)
	if len(list) == 0 {
		return []statusCause{required(prefix + ".containers")}
	}
	var causes []statusCause
	for i, c := range list {
		cm, _ := c.(map[string]any)
		field := fmt.Sprintf("%s.containers[%d]", prefix, i)
		if str(cm["name"]) == "" {
			causes = append(causes, required(field+".name"))
		}
		if str(cm["image"]) == "" {
			causes = append(causes, required(field+".image"))
		}
	}
	return causes
}

func validatePod(obj map[string]any) []statusCause {
	causes := validateName(str(path(obj, "metadata", "name")), false)
	spec, _ := obj["spec"].(map[string]any)
	return append(causes, validateContainers(spec, "spec")...)
}

func validateDeployment(obj map[string]any) []statusCause {
	causes := validateName(str(path(obj, "metadata", "name")), false)
	spec, _ := obj["spec"].(map[string]any)
	if n, ok := intValue(spec["replicas"]); ok && n < 0 {
		causes = append(causes, invalid("spec.replicas", "Invalid value: must be greater than or equal to 0"))
	}
	match, _ := path(spec, "selector", "matchLabels").(map[string]any)
	if len(match) == 0 {
		causes = append(causes, required("spec.selector"))
	}
	tpl, _ := spec["template"].(map[string]any)
	tplLabels, _ := path(tpl, "metadata", "labels").(map[string]any)
	for k, v := range match {
		if tv, ok := tplLabels[k]; !ok || tv != v {
			causes = append(causes, invalid("spec.template.metadata.labels", "Invalid value: `selector` does not match template `labels`"))
			break
		}
	}
	tplSpec, _ := tpl["spec"].(map[string]any)
	return append(causes, validateContainers(tplSpec, "spec.template.spec")...)
}

func defaultNamespace(obj map[string]any) {
	meta := metadata(obj)
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	labels["kubernetes.io/metadata.name"] = meta["name"]
	meta["labels"] = labels
	obj["spec"] = map[string]any{"finalizers": []any{"kubernetes"}}
	obj["status"] = map[string]any{"phase": "Active"}
}

func defaultPod(obj map[string]any) {
	spec, _ := obj["spec"].(map[string]any)
	if _, ok := spec["restartPolicy"]; !ok {
		spec["restartPolicy"] = "Always"
	}
	obj["status"] = map[string]any{"phase": "Pending"}
}

func defaultDeployment(obj map[string]any) {
	spec, _ := obj["spec"].(map[string]any)
	if _, ok := spec["replicas"]; !ok {
		spec["replicas"] = int64(defaultReplicas)
	}
	obj["status"] = deploymentStatus(spec, 1)
}

// deploymentStatus is what the mock reports for a deployment: it runs no
// controller, so every desired replica is reported ready at once.
func deploymentStatus(spec map[string]any, generation int64) map[string]any {
	n, _ := intValue(spec["replicas"])
	return map[string]any{
		"observedGeneration": generation, "replicas": n, "updatedReplicas": n, "readyReplicas": n, "availableReplicas": n,
	}
}

func metadata(obj map[string]any) map[string]any {
	m, _ := obj["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		obj["metadata"] = m
	}
	return m
}

// path walks nested objects and returns nil when any step is missing.
func path(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func labelsOf(obj map[string]any) map[string]string {
	raw, _ := path(obj, "metadata", "labels").(map[string]any)
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = str(v)
	}
	return out
}

func generateName(prefix string) string {
	b := make([]byte, generatedSuffix)
	rnd := randomHex(generatedSuffix)
	for i := range b {
		b[i] = suffixAlphabet[int(rnd[i])%len(suffixAlphabet)]
	}
	return prefix + string(b)
}

func (d *resourceDef) listKind() string { return d.kind + "List" }

// intValue reads a JSON number (a json.Number when decoded with UseNumber) as an integer.
func intValue(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int64:
		return n, true
	}
	return 0, false
}
