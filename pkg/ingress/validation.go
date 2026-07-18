package ingress

import (
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
)

// ValidateSupported verifies that an Ingress fits the subset currently modeled
// by the CMP/F5 extension. The current deployer creates one VIP and one virtual
// server backed by one Kubernetes Service NodePort, so all Ingress backend
// references must resolve to the same Service and backend port.
func ValidateSupported(ing *networkingv1.Ingress) error {
	if ing == nil {
		return fmt.Errorf("ingress must not be nil")
	}

	var first *backendRef
	consume := func(be networkingv1.IngressBackend) error {
		if be.Resource != nil {
			return fmt.Errorf("resource backends are not supported")
		}
		if be.Service == nil {
			return fmt.Errorf("backend service reference is required")
		}
		ref := backendRefFromService(be.Service)
		if first == nil {
			first = &ref
			return nil
		}
		if !first.equal(ref) {
			return fmt.Errorf("multiple backend services or ports are not supported: first=%s current=%s", first.String(), ref.String())
		}
		return nil
	}

	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if err := consume(path.Backend); err != nil {
				return err
			}
		}
	}
	if ing.Spec.DefaultBackend != nil {
		if err := consume(*ing.Spec.DefaultBackend); err != nil {
			return err
		}
	}
	if first == nil {
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
