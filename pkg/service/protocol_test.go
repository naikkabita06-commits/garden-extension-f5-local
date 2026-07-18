package service

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestMapK8sProtocolToCMP(t *testing.T) {
	tests := []struct {
		name  string
		proto corev1.Protocol
		port  int32
		want  string
	}{
		{name: "udp", proto: corev1.ProtocolUDP, port: 53, want: "UDP"},
		{name: "http", proto: corev1.ProtocolTCP, port: 80, want: "HTTP"},
		{name: "https", proto: corev1.ProtocolTCP, port: 443, want: "HTTPS"},
		{name: "tcp", proto: corev1.ProtocolTCP, port: 9000, want: "TCP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MapK8sProtocolToCMP(tt.proto, tt.port); got != tt.want {
				t.Fatalf("MapK8sProtocolToCMP() = %q, want %q", got, tt.want)
			}
		})
	}
}
