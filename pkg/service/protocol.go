package service

import corev1 "k8s.io/api/core/v1"

// MapK8sProtocolToCMP maps a Kubernetes Service port protocol and frontend
// port to the CMP protocol string used by the desired-state builders.
func MapK8sProtocolToCMP(p corev1.Protocol, port int32) string {
	switch p {
	case corev1.ProtocolUDP:
		return "UDP"
	case corev1.ProtocolTCP:
		switch port {
		case 80, 8080:
			return "HTTP"
		case 443, 8443:
			return "HTTPS"
		default:
			return "TCP"
		}
	default:
		return "TCP"
	}
}
