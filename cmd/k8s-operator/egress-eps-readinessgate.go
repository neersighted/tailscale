// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"
	xslices "golang.org/x/exp/slices"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"tailscale.com/tstime"
)

type egressReadinessReconciler struct {
	client.Client
	logger      *zap.SugaredLogger
	tsNamespace string
	clock       tstime.Clock
}

func (er *egressReadinessReconciler) Reconcile(ctx context.Context, req reconcile.Request) (res reconcile.Result, err error) {
	l := er.logger.With("Pod", req.NamespacedName)
	l.Debugf("starting reconcile")
	defer l.Debugf("reconcile finished")

	pod := new(corev1.Pod)
	err = er.Get(ctx, req.NamespacedName, pod)
	if apierrors.IsNotFound(err) {
		l.Debugf("Pod not found")
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get Pod: %w", err)
	}
	if !pod.DeletionTimestamp.IsZero() {
		l.Debugf("Pod is being deleted")
		return res, nil
	}
	// TODO: custom cluster domain
	proxyGroupName := pod.Labels[LabelParentName]
	// Retrieve the desired tailnet service configuration from the ConfigMap.
	_, cfgs, err := egressSvcsConfigs(ctx, er.Client, proxyGroupName, er.tsNamespace)
	if err != nil {
		return res, fmt.Errorf("error retrieving tailnet services configuration: %w", err)
	}

	// TODO: test with nil
	for name := range *cfgs {
		extSvcNs, extSvcName, err := extNsNameFromClusterSvcName(name)
		if err != nil {
			// This is not an error that could be fixed by reconcile
			l.Errorf("[unexpected] unable to determine ClusterIP Service namespace, name from %s", name)
		}
		epsLabels := map[string]string{
			LabelManaged:         "true",
			LabelParentType:      "svc",
			LabelParentName:      extSvcName,
			LabelParentNamespace: extSvcNs,
			labelProxyGroup:      proxyGroupName,
			labelSvcType:         typeEgress,
		}
		eps, err := getSingleObject[discoveryv1.EndpointSlice](ctx, er.Client, er.tsNamespace, epsLabels)
		if apierrors.IsNotFound(err) {
			l.Infof("Endpointslice for %s not found, waiting", name)
			return res, nil
		}
		if err != nil {
			return res, fmt.Errorf("error retrieving EndpointSlice for %s: %w", name, err)
		}
		found := false
		// TODO: better check once we support IPv6
		// TODO: probably can use some of those fancy slice expressions instead of these many loops
		for _, ep := range eps.Endpoints {
			for _, addr := range ep.Addresses {
				for _, podIP := range pod.Status.PodIPs {
					if strings.EqualFold(podIP.IP, addr) {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			l.Infof("Routing not yet set up for %s", name)
			return res, nil
		}
		// get the endpointslice for this target
		// check if it contains IP address of the Pod
	}
	idx := xslices.IndexFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == "tailscale.com/egress-services"
	})
	if idx != -1 {
		l.Debugf("condition exists")
		return res, nil
	}
	err, cont := er.previousReady(ctx, pod.Name, pod.Namespace, proxyGroupName, l)
	if err != nil {
		return res, err
	}
	if !cont {
		l.Infof("Pod not yet ready")
		return reconcile.Result{Requeue: true}, nil
	}
	l.Infof("Pod ready")
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:               "tailscale.com/egress-services",
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Time{Time: er.clock.Now()},
	})
	if err := er.Status().Update(ctx, pod); err != nil {
		return res, fmt.Errorf("error updating Pod status: %v", err)
	}
	return res, nil
}

func (er egressReadinessReconciler) previousReady(ctx context.Context, name, ns, pgName string, l *zap.SugaredLogger) (error, bool) {
	ordinal, _ := strings.CutPrefix(name, pgName)
	next, err := strconv.Atoi(ordinal)
	if err != nil {
		return err, false
	}
	next++

	pod := &corev1.Pod{}
	if err := er.Get(ctx, types.NamespacedName{Namespace: ns, Name: fmt.Sprintf("%s-%d", name, next)}, pod); err != nil {
		l.Infof("error finding next Pod: %v", err)
		return nil, true
	}
	podDNSName := fmt.Sprintf("http://%s.%s.%s.svc.cluster.local:9002/healthz", pod.Name, pgName, er.tsNamespace)
	l.Infof("calling Pod's health check at %s", podDNSName)
	resp, err := http.Get(podDNSName)
	if err != nil {
		l.Infof("error calling Pod's health check endpoint: %v", err)
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		l.Infof("Expected Pod's health check to return 200, got: %v", err)
		return nil, false
	}
	return nil, true
}
