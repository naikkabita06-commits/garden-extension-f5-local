package ingress

import (
	"fmt"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
)

// ValidateSupported verifies that an Ingress fits the subset that is currently
// reconciled end-to-end by the CMP/F5 deployer. Design-level routing/TLS model
// types may exist, but unsupported runtime features are rejected here so the
// controller never silently provisions a partial or misleading data plane.
func ValidateSupported(ing *networkingv1.Ingress) error {
	if err := validateIngressShape(ing); err != nil {
		return err
	}
	backendRefs := map[string]backendRef{}
	consume := func(be networkingv1.IngressBackend) {
		ref := backendRefFromService(be.Service)
		backendRefs[ref.String()] = ref
	}
	for _, rule := range ing.Spec.Rules {
		for _, path := range rule.HTTP.Paths {
			consume(path.Backend)
		}
	}
	if ing.Spec.DefaultBackend != nil {
		consume(*ing.Spec.DefaultBackend)
	}
	if len(backendRefs) > 1 {
		return fmt.Errorf("multiple backend services or ports require routing-rule and pool deployment support that is not yet enabled")
	}
	return nil
}

// validateIngressShape checks Kubernetes and routing semantics shared by the
// legacy single-Ingress path and the group graph builder. It intentionally
// does not reject TLS or multiple backends: those are valid desired-state
// constructs and are represented by the group desired-state builder.
func validateIngressShape(ing *networkingv1.Ingress) error {
	if ing == nil {
		return fmt.Errorf("ingress must not be nil")
	}

	seenRules := map[string]backendRef{}
	backendRefs := map[string]backendRef{}
	backendCount := 0
	consume := func(be networkingv1.IngressBackend, routeKey string) error {
		if be.Resource != nil {
			return fmt.Errorf("resource backends are not supported")
		}
		if be.Service == nil || strings.TrimSpace(be.Service.Name) == "" {
			return fmt.Errorf("backend service reference is required")
		}
		if be.Service.Port.Name == "" && be.Service.Port.Number == 0 {
			return fmt.Errorf("backend service port is required for %s", be.Service.Name)
		}
		backendCount++
		ref := backendRefFromService(be.Service)
		backendRefs[ref.String()] = ref
		if previous, ok := seenRules[routeKey]; ok && !previous.equal(ref) {
			return fmt.Errorf("conflicting backends for duplicate host/path %q: first=%s current=%s", routeKey, previous.String(), ref.String())
		}
		seenRules[routeKey] = ref
		return nil
	}

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			return fmt.Errorf("non-HTTP ingress rules are not supported")
		}
		for _, path := range rule.HTTP.Paths {
			if path.Path == "" {
				return fmt.Errorf("ingress path must not be empty")
			}
			if path.PathType == nil {
				return fmt.Errorf("ingress pathType is required for %s", path.Path)
			}
			switch *path.PathType {
			case networkingv1.PathTypeExact, networkingv1.PathTypePrefix:
			case networkingv1.PathTypeImplementationSpecific:
				return fmt.Errorf("ImplementationSpecific pathType is not supported for %s", path.Path)
			default:
				return fmt.Errorf("unsupported pathType for %s", path.Path)
			}
			if err := consume(path.Backend, routeKey(rule.Host, path.Path, *path.PathType)); err != nil {
				return err
			}
		}
	}
	if ing.Spec.DefaultBackend != nil {
		if err := consume(*ing.Spec.DefaultBackend, "<default>"); err != nil {
			return err
		}
	}
	for _, tls := range ing.Spec.TLS {
		if strings.TrimSpace(tls.SecretName) == "" {
			return fmt.Errorf("tls.secretName is required when TLS is configured")
		}
	}
	if backendCount == 0 {
		return fmt.Errorf("at least one service backend is required")
	}
	return nil
}

type backendRef struct {
	serviceName string
	portName    string
	portNumber  int32
}

func backendRefFromService(svc *networkingv1.IngressServiceBackend) backendRef {
	return backendRef{serviceName: svc.Name, portName: svc.Port.Name, portNumber: svc.Port.Number}
}

func (r backendRef) equal(other backendRef) bool {
	return r.serviceName == other.serviceName && r.portName == other.portName && r.portNumber == other.portNumber
}

func (r backendRef) String() string {
	if r.portName != "" {
		return fmt.Sprintf("%s:%s", r.serviceName, r.portName)
	}
	return fmt.Sprintf("%s:%d", r.serviceName, r.portNumber)
}

func routeKey(host, path string, pathType networkingv1.PathType) string {
	return strings.ToLower(strings.TrimSpace(host)) + "|" + string(pathType) + "|" + path
}
