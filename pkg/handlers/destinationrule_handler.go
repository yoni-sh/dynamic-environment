/*
Copyright 2023 Riskified Ltd

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handlers

import (
	"context"
	goerrors "errors"
	"fmt"

	"github.com/go-logr/logr"
	riskifiedv1alpha1 "github.com/riskified/dynamic-environment/api/v1alpha1"
	"github.com/riskified/dynamic-environment/pkg/helpers"
	"github.com/riskified/dynamic-environment/pkg/watches"
	istioapi "istio.io/api/networking/v1alpha3"
	istionetwork "istio.io/client-go/pkg/apis/networking/v1alpha3"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A handler for managing DestinationRule manipulations.
type DestinationRuleHandler struct {
	client.Client
	// The unique name of the target DestinationRule
	UniqueName string
	// The unique version of the target DestinationRule
	UniqueVersion string
	// The namespace of the target DestinationRule
	Namespace string
	// The version label
	VersionLabel string
	// The version that gets the default route
	DefaultVersion string
	// Status handler (to be able to update status)
	StatusHandler *DynamicEnvStatusHandler
	// The host name of the service that points to the Deployment specified in
	// the subset.
	ServiceHosts []string
	// The name/nmespace of the DynamicEnv that launches this DestinationRule
	Owner types.NamespacedName
	Log   logr.Logger
	Ctx   context.Context

	ignoredMissing []string
	activeHosts    []string
}

// Handles creation and manipulation of related DestinationRules.
func (h *DestinationRuleHandler) Handle() error {
	for _, serviceHost := range h.ServiceHosts {
		found := &istionetwork.DestinationRule{}
		drName := h.calculateDRName(serviceHost)
		if err := h.Get(h.Ctx, types.NamespacedName{Name: drName, Namespace: h.Namespace}, found); err != nil {
			if errors.IsNotFound(err) {
				if err := h.createMissingDestinationRule(drName, serviceHost); err != nil {
					return err
				}
				continue
			}

			return fmt.Errorf("error locating existing destination rule by name (%s): %w", serviceHost, err)
		}
		h.activeHosts = append(h.activeHosts, serviceHost)
	}

	if len(h.activeHosts) == 0 {
		return fmt.Errorf("no base destination rules were found for subset: %s", h.UniqueName)
	}

	return nil
}

// GetStatus here can only return missing or running is there is no real status
// for DestinationRule, just whether it exists or missing.
func (h *DestinationRuleHandler) GetStatus() (statuses []riskifiedv1alpha1.ResourceStatus, err error) {

	genStatus := func(name string, s riskifiedv1alpha1.LifeCycleStatus) riskifiedv1alpha1.ResourceStatus {
		return riskifiedv1alpha1.ResourceStatus{
			Name:      name,
			Namespace: h.Namespace,
			Status:    s,
		}
	}

	for _, sh := range h.ServiceHosts {
		found := &istionetwork.DestinationRule{}
		drName := h.calculateDRName(sh)
		if err := h.Get(h.Ctx, types.NamespacedName{Name: drName, Namespace: h.Namespace}, found); err != nil {
			if errors.IsNotFound(err) {
				if helpers.StringSliceContains(sh, h.ignoredMissing) {
					statuses = append(statuses, genStatus(drName, riskifiedv1alpha1.IgnoredMissingDR))
					continue
				}
				statuses = append(statuses, genStatus(drName, riskifiedv1alpha1.Missing))
				continue
			}
			return statuses, fmt.Errorf("error locating existing destination rule by name (%s): %w", drName, err)
		}
		statuses = append(statuses, genStatus(drName, riskifiedv1alpha1.Running))
	}

	return statuses, nil
}

func (h *DestinationRuleHandler) ApplyStatus(statuses []riskifiedv1alpha1.ResourceStatus) error {
	for _, rs := range statuses {
		if err := h.StatusHandler.AddDestinationRuleStatusEntry(h.UniqueName, rs); err != nil {
			return err
		}
	}
	return nil
}

func (h *DestinationRuleHandler) GetSubset() string {
	return h.UniqueName
}

func (h *DestinationRuleHandler) GetHosts() []string {
	return h.activeHosts
}

func (h *DestinationRuleHandler) createMissingDestinationRule(destinationRuleName, serviceHost string) error {
	if err := h.setStatus(h.UniqueName, destinationRuleName, riskifiedv1alpha1.Initializing); err != nil {
		return fmt.Errorf("failed to update status (prior to launching destination rule: %s): %w", serviceHost, err)
	}
	if err := h.createOverridingDestinationRule(destinationRuleName, serviceHost); err != nil {
		if goerrors.As(err, &IgnoredMissing{}) {
			h.ignoredMissing = append(h.ignoredMissing, serviceHost)
			h.Log.Info("Added hostname to list of ignored missing", "hostname", serviceHost)
		} else {
			return fmt.Errorf("creating destination rule for '%s': %w", serviceHost, err)
		}
	} else {
		h.activeHosts = append(h.activeHosts, serviceHost)
	}
	return nil
}

func (h *DestinationRuleHandler) createOverridingDestinationRule(drName, serviceHost string) error {
	newDestinationRule, err := h.generateOverridingDestinationRule(serviceHost)
	if err != nil {
		return fmt.Errorf("creating overriding destination rule: %w", err)
	}
	h.Log.Info("Deploying newly created destination rule", "destination rule name", h.UniqueName, "service-host", drName)
	watches.AddToAnnotation(h.Owner, newDestinationRule)
	if err = h.Create(h.Ctx, newDestinationRule); err != nil {
		return fmt.Errorf("error deploying new destination rule version=%q service-host=%q: %w", h.UniqueName, drName, err)
	}
	return nil
}

func (h *DestinationRuleHandler) generateOverridingDestinationRule(serviceHost string) (*istionetwork.DestinationRule, error) {
	originalDestinationRule, err := h.locateDestinationRuleByHostname(serviceHost)
	if err != nil {
		return nil, fmt.Errorf("locating default destination rule for '%s': %w", h.ServiceHosts, err)
	}
	subset := &istioapi.Subset{
		Labels: map[string]string{h.VersionLabel: h.UniqueVersion},
		Name:   h.UniqueVersion,
	}
	newDestinationRule := &istionetwork.DestinationRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.calculateDRName(serviceHost),
			Namespace: h.Namespace,
			Labels: map[string]string{
				h.VersionLabel: h.UniqueVersion,
			},
		},
		Spec: istioapi.DestinationRule{
			Host: originalDestinationRule.Spec.Host,
			Subsets: []*istioapi.Subset{
				subset,
			},
		},
	}
	return newDestinationRule, nil
}

func (h *DestinationRuleHandler) locateDestinationRuleByHostname(hostName string) (*istionetwork.DestinationRule, error) {
	destinationRules := &istionetwork.DestinationRuleList{}
	if err := h.List(h.Ctx, destinationRules, client.InNamespace(h.Namespace)); err != nil {
		return nil, fmt.Errorf("error listing existing destination rules: %w", err)
	}
	for _, dr := range destinationRules.Items {
		if helpers.MatchNamespacedHost(hostName, h.Namespace, dr.Spec.Host, dr.Namespace) {
			for _, s := range dr.Spec.Subsets {
				if s.Labels[h.VersionLabel] == h.DefaultVersion {
					return dr, nil
				}
			}
		}
	}
	h.Log.Info("Couldn't find DestinationRule per hostname with default version", "default-version",
		h.VersionLabel, "namespace", h.Namespace, "hostname", hostName)
	return nil, IgnoredMissing{}
}

func (h *DestinationRuleHandler) setStatus(subset, drName string, status riskifiedv1alpha1.LifeCycleStatus) error {
	currentState := riskifiedv1alpha1.ResourceStatus{
		Name:      drName,
		Namespace: h.Namespace,
		Status:    status,
	}
	if err := h.StatusHandler.AddDestinationRuleStatusEntry(subset, currentState); err != nil {
		return err
	}
	return nil
}

func (h *DestinationRuleHandler) calculateDRName(serviceHost string) string {
	return h.UniqueName + "-" + serviceHost
}
