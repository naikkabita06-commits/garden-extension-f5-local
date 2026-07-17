// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ControllerConfiguration defines the configuration for the F5 extension.
type ControllerConfiguration struct {
	metav1.TypeMeta

	// F5Config contains F5-specific configuration
	F5Config F5Config
}

// F5Config contains the F5 BIG-IP configuration
type F5Config struct {
	// Endpoint is the F5 BIG-IP management endpoint
	Endpoint string
	// Partition is the F5 partition to use
	Partition string
}
