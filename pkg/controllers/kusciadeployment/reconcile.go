// Copyright 2023 Ant Group Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//nolint:dupl
package kusciadeployment

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/net/context"
	"google.golang.org/protobuf/encoding/protojson"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/secretflow/kuscia/pkg/common"
	kusciav1alpha1 "github.com/secretflow/kuscia/pkg/crd/apis/kuscia/v1alpha1"
	"github.com/secretflow/kuscia/pkg/utils/nlog"
	"github.com/secretflow/kuscia/proto/api/v1alpha1/appconfig"
)

const (
	configTemplateVolumeName = "config-template"
	kusciaDeploymentName     = "KusciaDeployment"
)

// ProcessKusciaDeployment processes kuscia deployment resource.
func (c *Controller) ProcessKusciaDeployment(ctx context.Context, kd *kusciav1alpha1.KusciaDeployment) (err error) {
	conditionNeedUpdate, err := c.updateKusciaDeploymentAnnotations(kd)
	if err != nil {
		nlog.Errorf("UpdateKusciaDeploymentSpec kd=%s/%s failed: %s", kd.Namespace, kd.Name, err)
		return err
	}

	preKdStatus := kd.Status.DeepCopy()
	partyKitInfos, kitNeedUpdate, err := c.buildPartyKitInfos(kd)
	if err != nil {
		return c.handleError(ctx, partyKitInfos, preKdStatus, kd, err)
	}

	// We update the spec and status separately.
	if conditionNeedUpdate || kitNeedUpdate {
		_, err = c.kusciaClient.KusciaV1alpha1().KusciaDeployments(kd.Namespace).Update(ctx, kd, metav1.UpdateOptions{})
		if err != nil && !k8serrors.IsConflict(err) {
			return fmt.Errorf("failed to updating kuscia deployment %v, %v", kd.Name, err)
		}
		return nil
	}

	if err = c.syncResources(ctx, partyKitInfos); err != nil {
		return c.handleError(ctx, partyKitInfos, preKdStatus, kd, err)
	}

	if c.refreshPartyDeploymentStatuses(kd, partyKitInfos) {
		return c.updateKusciaDeploymentStatus(ctx, kd)
	}

	return nil
}

func (c *Controller) updateKusciaDeploymentAnnotations(kd *kusciav1alpha1.KusciaDeployment) (bool, error) {
	generatedAnnotations, err := c.computeExceptGeneratedAnnotations(kd)
	if err != nil {
		return false, err
	}

	return c.mergeAnnotations(kd, generatedAnnotations), nil
}

// computeExceptGeneratedAnnotations calculates the annotations that should be automatically generated for the KusciaDeployment.
func (c *Controller) computeExceptGeneratedAnnotations(kd *kusciav1alpha1.KusciaDeployment) (map[string]string, error) {
	annotations := make(map[string]string)
	// We collect inter conn protocols parties, and we use LabelInterConnKusciaParty only now.
	interConnParties := make(map[string][]string)
	isInitiatorController, err := c.isInitiatorController(kd)
	if err != nil {
		return nil, err
	}
	if isInitiatorController {
		annotations[common.InitiatorAnnotationKey] = kd.Spec.Initiator
		annotations[common.SelfClusterAsInitiatorAnnotationKey] = "true"
		for _, p := range kd.Spec.Parties {
			partyDomain, getErr := c.domainLister.Get(p.DomainID)
			if getErr != nil {
				return nil, getErr
			}
			if partyDomain.Spec.Role == kusciav1alpha1.Partner {
				interConnProtocol := kusciav1alpha1.InterConnKuscia
				if len(partyDomain.Spec.InterConnProtocols) != 0 {
					interConnProtocol = partyDomain.Spec.InterConnProtocols[0]
				}
				if interConnParties[string(interConnProtocol)] == nil {
					interConnParties[string(interConnProtocol)] = []string{}
				}
				interConnParties[string(interConnProtocol)] =
					append(interConnParties[string(interConnProtocol)], partyDomain.Name)
			}
		}
		interConnKusciaParties := interConnParties[string(kusciav1alpha1.InterConnKuscia)]
		if interConnKusciaParties != nil {
			annotations[common.InterConnKusciaPartyAnnotationKey] = strings.Join(interConnKusciaParties, "_")
		}
	}

	selfParties, err := c.selfParties(kd)
	if err != nil {
		return nil, err
	}
	selfPartiesDomainIDs := make([]string, 0)
	for _, p := range selfParties {
		selfPartiesDomainIDs = append(selfPartiesDomainIDs, p.DomainID)
	}
	annotations[common.InterConnSelfPartyAnnotationKey] = strings.Join(selfPartiesDomainIDs, "_")

	return annotations, nil
}

// mergeAnnotations will merge annotations into the KusciaDeployment.
// If it returns true, it indicates that an update is required for the KusciaDeployment.
func (c *Controller) mergeAnnotations(kd *kusciav1alpha1.KusciaDeployment, annotations map[string]string) bool {
	updated := false
	for k, newV := range annotations {
		if oldV, exists := kd.Annotations[k]; !exists || oldV != newV {
			if kd.Annotations == nil {
				kd.Annotations = make(map[string]string)
			}
			kd.Annotations[k] = newV
			updated = true
		}
	}
	return updated
}

func (c *Controller) refreshPartyDeploymentStatuses(kd *kusciav1alpha1.KusciaDeployment, partyKitInfos []*PartyKitInfo) bool {
	updated := false
	if kd.Status.TotalParties == 0 {
		updated = true
		kd.Status.TotalParties = len(kd.Spec.Parties)
	}

	if kd.Status.PartyDeploymentStatuses == nil {
		kd.Status.PartyDeploymentStatuses = make(map[string]map[string]*kusciav1alpha1.KusciaDeploymentPartyStatus)
	}

	for _, partyKitInfo := range partyKitInfos {
		if partyKitInfo.domainID == "" || partyKitInfo.dkInfo == nil || partyKitInfo.dkInfo.deploymentName == "" {
			continue
		}

		deployment, err := c.deploymentLister.Deployments(partyKitInfo.domainID).Get(partyKitInfo.dkInfo.deploymentName)
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				nlog.Warnf("Failed to update party deployment %v/%v status for kuscia deployment %v, %v",
					partyKitInfo.domainID, partyKitInfo.dkInfo.deploymentName, kd.Name, err)
			}
			continue
		}

		changed := refreshPartyDeploymentStatus(kd.Status.PartyDeploymentStatuses, deployment, partyKitInfo.role)
		if changed {
			updated = true
		}
	}

	availableParties := 0
	hasPartialAvailableParty := false
	progressingParties := 0
	for domainID, partyDeploymentStatus := range kd.Status.PartyDeploymentStatuses {
		for kdName, v := range partyDeploymentStatus {
			if v.Phase == kusciav1alpha1.KusciaDeploymentPhaseAvailable {
				availableParties++
				continue
			}

			if v.Phase == kusciav1alpha1.KusciaDeploymentPhasePartialAvailable {
				hasPartialAvailableParty = true
				availableParties++
				continue
			}
			if v.Phase == kusciav1alpha1.KusciaDeploymentPhaseProgressing {
				progressingParties++
				// Get the reason and message of the pod below kd
				for _, partyKit := range partyKitInfos {
					if partyKit.domainID == domainID {
						reason, message, err := c.getPodInfoByKd(kdName, domainID)
						if err != nil {
							nlog.Warnf("Failed to get pod info for kd %v/%v: %v", domainID, kdName, err)
						}
						kd.Status.Reason = fmt.Sprintf("domainID: %s, reason: %s", domainID, reason)
						kd.Status.Message = message
						break
					}
				}
				continue
			}
		}
	}

	if kd.Status.AvailableParties != availableParties {
		kd.Status.AvailableParties = availableParties
		updated = true
	}
	kdOriginStatusPhase := kd.Status.Phase

	//When one of the parties is unavailable, the overall state should be progressing
	if progressingParties > 0 {
		kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseProgressing
		updated = true
	}

	if availableParties == kd.Status.TotalParties {
		if hasPartialAvailableParty {
			kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhasePartialAvailable
		} else {
			kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseAvailable
		}
	}
	if kdOriginStatusPhase != kd.Status.Phase {
		updated = true
		if kd.Status.Phase == kusciav1alpha1.KusciaDeploymentPhaseAvailable || kd.Status.Phase == kusciav1alpha1.KusciaDeploymentPhasePartialAvailable {
			kd.Status.Reason = ""
			kd.Status.Message = ""
		}
	}
	return updated
}

func (c *Controller) getPodInfoByKd(deploymentName, namespace string) (string, string, error) {
	deployment, err := c.deploymentLister.Deployments(namespace).Get(deploymentName)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			deployment, err = c.kubeClient.AppsV1().Deployments(namespace).Get(context.Background(), deploymentName, metav1.GetOptions{})
			if err != nil && k8serrors.IsNotFound(err) {
				message := fmt.Sprintf("kd's deployment: %s not found", deploymentName)
				return message, message, nil
			} else {
				return "", "", fmt.Errorf("get kd's deployment error: %v", err)
			}
		} else {
			return "", "", fmt.Errorf("get kd's deployment error: %v", err)
		}
	}

	if deployment.Spec.Selector == nil || deployment.Spec.Selector.MatchLabels == nil {
		message := fmt.Sprintf("kd's deployment: %s labels selector is nil", deploymentName)
		return message, message, nil
	}
	labelSelector := labels.Set(deployment.Spec.Selector.MatchLabels).AsSelector().String()

	pods, err := c.kubeClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			message := fmt.Sprintf("kd's pods: %s not found", deploymentName)
			return message, message, nil
		} else {
			return "", "", fmt.Errorf("get kd's pods error: %v", err)
		}
	}
	if len(pods.Items) == 0 {
		message := fmt.Sprintf("kd's pods: %s not found", deploymentName)
		return message, message, nil
	}
	for _, pod := range pods.Items {
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil {
				return containerStatus.State.Terminated.Reason, containerStatus.State.Terminated.Message, nil
			}
		}
	}
	return "", "", nil
}

func refreshPartyDeploymentStatus(partyDeploymentStatuses map[string]map[string]*kusciav1alpha1.KusciaDeploymentPartyStatus, deployment *appsv1.Deployment, role string) bool {
	curDepStatus := &kusciav1alpha1.KusciaDeploymentPartyStatus{
		Phase:               kusciav1alpha1.KusciaDeploymentPhaseProgressing,
		Role:                role,
		Replicas:            deployment.Status.Replicas,
		UpdatedReplicas:     deployment.Status.UpdatedReplicas,
		AvailableReplicas:   deployment.Status.AvailableReplicas,
		UnavailableReplicas: deployment.Status.UnavailableReplicas,
		Conditions:          deployment.Status.Conditions,
		CreationTimestamp:   &deployment.CreationTimestamp,
	}

	if curDepStatus.AvailableReplicas > 0 {
		if curDepStatus.AvailableReplicas >= curDepStatus.Replicas {
			curDepStatus.Phase = kusciav1alpha1.KusciaDeploymentPhaseAvailable
		} else {
			curDepStatus.Phase = kusciav1alpha1.KusciaDeploymentPhasePartialAvailable
		}
	}

	partyDepStatuses, ok := partyDeploymentStatuses[deployment.Namespace]
	if !ok {
		partyDepStatus := map[string]*kusciav1alpha1.KusciaDeploymentPartyStatus{
			deployment.Name: curDepStatus,
		}
		partyDeploymentStatuses[deployment.Namespace] = partyDepStatus
		return true
	}

	depStatus, ok := partyDepStatuses[deployment.Name]
	if !ok {
		partyDepStatuses[deployment.Name] = curDepStatus
		return true
	}

	if !reflect.DeepEqual(depStatus, curDepStatus) {
		partyDepStatuses[deployment.Name] = curDepStatus
		return true
	}
	return false
}

func (c *Controller) syncResources(ctx context.Context, partyKitInfos []*PartyKitInfo) (err error) {
	if err = c.syncService(ctx, partyKitInfos); err != nil {
		return err
	}

	if err = c.syncConfigMap(ctx, partyKitInfos); err != nil {
		return err
	}

	if err = c.syncDeployment(ctx, partyKitInfos); err != nil {
		return err
	}

	return nil
}

func (c *Controller) syncService(ctx context.Context, partyKitInfos []*PartyKitInfo) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("error syncing service, %v", err)
		}
	}()

	for _, partyKitInfo := range partyKitInfos {
		for portName, serviceName := range partyKitInfo.dkInfo.portService {
			svc, err := c.serviceLister.Services(partyKitInfo.domainID).Get(serviceName)
			if err != nil && k8serrors.IsNotFound(err) {
				svc, err = c.kubeClient.CoreV1().Services(partyKitInfo.domainID).Get(ctx, serviceName, metav1.GetOptions{})
			}
			if err != nil {
				if k8serrors.IsNotFound(err) {
					if err = c.createService(ctx, partyKitInfo, portName, serviceName); err != nil {
						partyKitInfo.kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseFailed
						partyKitInfo.kd.Status.Reason = string(createServiceFailed)
						partyKitInfo.kd.Status.Message = err.Error()
						return err
					}
					continue
				}
				return err
			}

			if svc.Labels == nil || svc.Labels[common.LabelKubernetesDeploymentName] != partyKitInfo.dkInfo.deploymentName {
				err = fmt.Errorf("domain %s service %s already exist, please ensure that the service name can't be duplicated", partyKitInfo.domainID, svc.Name)
				partyKitInfo.kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseFailed
				partyKitInfo.kd.Status.Reason = string(createServiceFailed)
				partyKitInfo.kd.Status.Message = err.Error()
				return err
			}
		}
	}
	return nil
}

func (c *Controller) createService(ctx context.Context, partyKitInfo *PartyKitInfo, portName, serviceName string) error {
	ctrPort, ok := partyKitInfo.dkInfo.ports[portName]
	if !ok {
		return fmt.Errorf("container port %q is not found in deployment %q", portName, partyKitInfo.dkInfo.deploymentName)
	}

	service, err := generateService(partyKitInfo, serviceName, ctrPort)
	if err != nil {
		return fmt.Errorf("failed to generate service %v/%v for deployment %v, %v", service.Namespace, service.Name, partyKitInfo.dkInfo.deploymentName, err)
	}

	if _, err = c.kubeClient.CoreV1().Services(service.Namespace).Create(ctx, service, metav1.CreateOptions{}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			nlog.Warnf("Service %v/%v is already exists, %v", service.Namespace, service.Name, err)
			return nil
		}
		return fmt.Errorf("failed to create service %v/%v for deployment %v, %v", service.Namespace, service.Name, partyKitInfo.dkInfo.deploymentName, err)
	}
	return nil
}

func generateService(partyKitInfo *PartyKitInfo, serviceName string, port kusciav1alpha1.ContainerPort) (*corev1.Service, error) {
	labels := map[string]string{
		common.LabelController:               kusciaDeploymentName,
		common.LabelPortScope:                string(port.Scope),
		common.LabelKusciaDeploymentUID:      string(partyKitInfo.kd.UID),
		common.LabelKusciaDeploymentName:     partyKitInfo.kd.Name,
		common.LabelKusciaOwnerNamespace:     common.KusciaCrossDomain,
		common.LabelKubernetesDeploymentName: partyKitInfo.dkInfo.deploymentName,
	}

	if port.Scope == kusciav1alpha1.ScopeCluster {
		labels[common.LabelLoadBalancer] = string(common.DomainRouteLoadBalancer)
	}

	annotations := map[string]string{
		common.InitiatorAnnotationKey:    partyKitInfo.kd.Spec.Initiator,
		common.ProtocolAnnotationKey:     string(port.Protocol),
		common.AccessDomainAnnotationKey: partyKitInfo.portAccessDomains[port.Name],
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName,
			Namespace:   partyKitInfo.domainID,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
			Selector: map[string]string{
				common.LabelKubernetesDeploymentName: partyKitInfo.dkInfo.deploymentName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     port.Name,
					Port:     port.Port,
					Protocol: corev1.ProtocolTCP,
					TargetPort: intstr.IntOrString{
						Type:   intstr.String,
						StrVal: port.Name,
					},
				},
			},
		},
	}

	return svc, nil
}

func (c *Controller) syncConfigMap(ctx context.Context, partyKitInfos []*PartyKitInfo) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("error syncing configmap, %v", err)
		}
	}()

	for _, partyKitInfo := range partyKitInfos {
		if len(partyKitInfo.configTemplates) > 0 && partyKitInfo.configTemplatesCMName != "" {
			cm, err := c.configMapLister.ConfigMaps(partyKitInfo.domainID).Get(partyKitInfo.configTemplatesCMName)
			if err != nil && k8serrors.IsNotFound(err) {
				cm, err = c.kubeClient.CoreV1().ConfigMaps(partyKitInfo.domainID).Get(ctx, partyKitInfo.configTemplatesCMName, metav1.GetOptions{})
			}
			if err != nil {
				if k8serrors.IsNotFound(err) {
					if err = c.createConfigMap(ctx, partyKitInfo); err != nil {
						partyKitInfo.kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseFailed
						partyKitInfo.kd.Status.Reason = string(createConfigMapFailed)
						partyKitInfo.kd.Status.Message = err.Error()
						return err
					}
					continue
				}
				return err
			}

			if cm.Labels == nil || cm.Labels[common.LabelKubernetesDeploymentName] != partyKitInfo.dkInfo.deploymentName {
				err = fmt.Errorf("domain %s configmap %s already exist, please ensure that the configmap name can't be duplicated", partyKitInfo.domainID, cm.Name)
				partyKitInfo.kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseFailed
				partyKitInfo.kd.Status.Reason = string(createConfigMapFailed)
				partyKitInfo.kd.Status.Message = err.Error()
				return err
			}
		}
	}
	return nil
}

func (c *Controller) createConfigMap(ctx context.Context, partyKitInfo *PartyKitInfo) error {
	cm := generateConfigMap(partyKitInfo)
	if _, err := c.kubeClient.CoreV1().ConfigMaps(partyKitInfo.domainID).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			nlog.Warnf("Configmap %v/%v is already exists, %v", cm.Namespace, cm.Name, err)
			return nil
		}
		return fmt.Errorf("failed to create configmap %v/%v for deployment %v, %v", cm.Namespace, cm.Name, partyKitInfo.dkInfo.deploymentName, err)
	}
	return nil
}

func generateConfigMap(partyKitInfo *PartyKitInfo) *corev1.ConfigMap {
	labels := map[string]string{
		common.LabelController:               kusciaDeploymentName,
		common.LabelKusciaDeploymentUID:      string(partyKitInfo.kd.UID),
		common.LabelKusciaDeploymentName:     partyKitInfo.kd.Name,
		common.LabelKusciaOwnerNamespace:     common.KusciaCrossDomain,
		common.LabelKubernetesDeploymentName: partyKitInfo.dkInfo.deploymentName,
	}

	annotations := map[string]string{
		common.InitiatorAnnotationKey: partyKitInfo.kd.Spec.Initiator,
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        partyKitInfo.configTemplatesCMName,
			Namespace:   partyKitInfo.domainID,
			Labels:      labels,
			Annotations: annotations,
		},
		Data: partyKitInfo.configTemplates,
	}
}

func (c *Controller) syncDeployment(ctx context.Context, partyKitInfos []*PartyKitInfo) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("error syncing deployment, %v", err)
		}
	}()

	for _, partyKitInfo := range partyKitInfos {
		deployment, err := c.deploymentLister.Deployments(partyKitInfo.domainID).Get(partyKitInfo.dkInfo.deploymentName)
		if err != nil && k8serrors.IsNotFound(err) {
			deployment, err = c.kubeClient.AppsV1().Deployments(partyKitInfo.domainID).Get(ctx, partyKitInfo.dkInfo.deploymentName, metav1.GetOptions{})
		}
		if err != nil {
			if k8serrors.IsNotFound(err) {
				if err = c.createDeployment(ctx, partyKitInfo); err != nil {
					partyKitInfo.kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseFailed
					partyKitInfo.kd.Status.Reason = string(createDeploymentFailed)
					partyKitInfo.kd.Status.Message = err.Error()
					return err
				}
				continue
			}
			return err
		}

		if deployment.Labels == nil || deployment.Labels[common.LabelKusciaDeploymentName] != partyKitInfo.kd.Name {
			err = fmt.Errorf("domain %s deployment %s already exist, please ensure that the deployment name can't be duplicated", partyKitInfo.domainID, deployment.Name)
			partyKitInfo.kd.Status.Phase = kusciav1alpha1.KusciaDeploymentPhaseFailed
			partyKitInfo.kd.Status.Reason = string(createDeploymentFailed)
			partyKitInfo.kd.Status.Message = err.Error()
			return err
		}

		if err = c.updateDeployment(ctx, partyKitInfo); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) createDeployment(ctx context.Context, partyKitInfo *PartyKitInfo) error {
	deployment, err := c.generateDeployment(partyKitInfo)
	if err != nil {
		return err
	}

	if _, err = c.kubeClient.AppsV1().Deployments(partyKitInfo.domainID).Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			nlog.Warnf("Deployment %v is already exists, %v", partyKitInfo.dkInfo.deploymentName, err)
			return nil
		}
		return fmt.Errorf("failed to create deployment %v/%v for deployment %v, %v", deployment.Namespace, deployment.Name, partyKitInfo.dkInfo.deploymentName, err)
	}
	return err
}

func (c *Controller) generateDeployment(partyKitInfo *PartyKitInfo) (*appsv1.Deployment, error) {
	selectorLabels := map[string]string{
		common.LabelController:               kusciaDeploymentName,
		common.LabelKusciaDeploymentUID:      string(partyKitInfo.kd.UID),
		common.LabelKusciaDeploymentName:     partyKitInfo.kd.Name,
		common.LabelKusciaOwnerNamespace:     common.KusciaCrossDomain,
		common.LabelKubernetesDeploymentName: partyKitInfo.dkInfo.deploymentName,
		common.LabelCommunicationRoleServer:  "true",
		common.LabelCommunicationRoleClient:  "true",
	}

	if partyKitInfo.kd.Labels != nil && partyKitInfo.kd.Labels[common.LabelKusciaDeploymentAppType] != "" {
		selectorLabels[common.LabelKusciaDeploymentAppType] = partyKitInfo.kd.Labels[common.LabelKusciaDeploymentAppType]
	}

	annotations := map[string]string{
		common.InitiatorAnnotationKey: partyKitInfo.kd.Spec.Initiator,
	}

	ns, err := c.namespaceLister.Get(partyKitInfo.domainID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate deployment %v, %v", partyKitInfo.dkInfo.deploymentName, err)
	}

	schedulerName := common.KusciaSchedulerName
	if ns.Labels != nil {
		if ns.Labels[common.LabelDomainRole] == string(kusciav1alpha1.Partner) {
			schedulerName = fmt.Sprintf("%v-%v", partyKitInfo.domainID, schedulerName)
		}
	}

	maxSurge := intstr.FromString("25%")
	maxUnavailable := intstr.FromString("25%")
	updateStrategy := &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       &maxSurge,
			MaxUnavailable: &maxUnavailable,
		},
	}
	if partyKitInfo.deployTemplate.Strategy != nil {
		updateStrategy = partyKitInfo.deployTemplate.Strategy
	}

	var affinity *corev1.Affinity
	if partyKitInfo.deployTemplate.Spec.Affinity != nil {
		affinity = partyKitInfo.deployTemplate.Spec.Affinity.DeepCopy()
		buildAffinity(affinity, partyKitInfo.dkInfo.deploymentName)
	} else {
		// If affinity is not set in AppImage, add the default PodAntiAffinity
		affinity = buildDefaultPodAntiAffinity(partyKitInfo.dkInfo.deploymentName)
	}

	automountServiceAccountToken := false
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        partyKitInfo.dkInfo.deploymentName,
			Namespace:   partyKitInfo.domainID,
			Labels:      selectorLabels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: partyKitInfo.deployTemplate.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Strategy: *updateStrategy,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: selectorLabels,
				},
				Spec: corev1.PodSpec{
					Affinity: affinity,
					Tolerations: []corev1.Toleration{
						{
							Key:      common.KusciaTaintTolerationKey,
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					NodeSelector: map[string]string{
						common.LabelNodeNamespace: partyKitInfo.domainID,
					},
					SchedulerName:                schedulerName,
					AutomountServiceAccountToken: &automountServiceAccountToken,
				},
			},
		},
	}
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	if partyKitInfo.dkInfo.imageID != "" {
		deployment.Spec.Template.Annotations[common.ImageIDAnnotationKey] = partyKitInfo.dkInfo.imageID
	}

	renderConfigTemplateVolume := false
	for _, ctr := range partyKitInfo.deployTemplate.Spec.Containers {
		if ctr.ImagePullPolicy == "" {
			ctr.ImagePullPolicy = corev1.PullIfNotPresent
		}

		if ctr.MetricProbe != nil {
			metricPath := ctr.MetricProbe.Path
			metricPortName := ctr.MetricProbe.Port
			if metricPath != "" && metricPortName != "" {
				if portInfo, ok := partyKitInfo.dkInfo.ports[metricPortName]; ok {
					deployment.Spec.Template.Annotations[common.MetricAnnotationKey] = "true"
					deployment.Spec.Template.Annotations[common.MetricPathAnnotationKey] = metricPath
					deployment.Spec.Template.Annotations[common.MetricPortAnnotationKey] = strconv.Itoa(int(portInfo.Port))
				} else {
					return nil, fmt.Errorf("metric port name %s not found for deployment %s", metricPortName, deployment.Name)
				}

			}
		}

		resCtr := corev1.Container{
			Name:                     ctr.Name,
			Image:                    partyKitInfo.dkInfo.image,
			Command:                  ctr.Command,
			Args:                     ctr.Args,
			WorkingDir:               ctr.WorkingDir,
			Env:                      ctr.Env,
			EnvFrom:                  ctr.EnvFrom,
			Resources:                ctr.Resources,
			LivenessProbe:            ctr.LivenessProbe,
			ReadinessProbe:           ctr.ReadinessProbe,
			StartupProbe:             ctr.StartupProbe,
			ImagePullPolicy:          ctr.ImagePullPolicy,
			SecurityContext:          ctr.SecurityContext,
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}

		for _, port := range ctr.Ports {
			namedPort, ok := partyKitInfo.dkInfo.ports[port.Name]
			if !ok {
				return nil, fmt.Errorf("port %s is not allocated for deployment %s", port.Name, partyKitInfo.dkInfo.deploymentName)
			}
			resPort := corev1.ContainerPort{
				Name:          port.Name,
				ContainerPort: namedPort.Port,
				Protocol:      corev1.ProtocolTCP,
			}

			resCtr.Ports = append(resCtr.Ports, resPort)
		}

		protoJSONOptions := protojson.MarshalOptions{EmitUnpopulated: true}
		clusterDefine, err := protoJSONOptions.Marshal(partyKitInfo.dkInfo.clusterDef)
		if err != nil {
			return nil, fmt.Errorf("failed to generate deployment %v, %v", partyKitInfo.dkInfo.deploymentName, err)
		}

		allocatedPorts, err := protoJSONOptions.Marshal(partyKitInfo.dkInfo.allocatedPorts)
		if err != nil {
			return nil, fmt.Errorf("failed to generate deployment %v, %v", partyKitInfo.dkInfo.deploymentName, err)
		}

		resCtr.Env = append(resCtr.Env, []corev1.EnvVar{
			{
				Name:  common.EnvDomainID,
				Value: partyKitInfo.domainID,
			},
			{
				Name:  common.EnvClusterDefine,
				Value: string(clusterDefine),
			},
			{
				Name:  common.EnvAllocatedPorts,
				Value: string(allocatedPorts),
			},
			{
				Name:  common.EnvInputConfig,
				Value: partyKitInfo.kd.Spec.InputConfig,
			},
		}...)

		portNumberEnvs := buildPortNumberEnvs(partyKitInfo.dkInfo.allocatedPorts)
		if len(portNumberEnvs) > 0 {
			resCtr.Env = append(resCtr.Env, portNumberEnvs...)
		}

		if partyKitInfo.kd.Labels != nil && partyKitInfo.kd.Labels[common.LabelKusciaDeploymentAppType] == string(common.ServingApp) {
			resCtr.Env = append(resCtr.Env, corev1.EnvVar{
				Name:  common.EnvServingID,
				Value: partyKitInfo.kd.Name,
			})
		}

		if len(ctr.ConfigVolumeMounts) > 0 && partyKitInfo.configTemplatesCMName != "" {
			renderConfigTemplateVolume = true
			for _, vm := range ctr.ConfigVolumeMounts {
				resCtr.VolumeMounts = append(resCtr.VolumeMounts, corev1.VolumeMount{
					Name:      configTemplateVolumeName,
					MountPath: vm.MountPath,
					SubPath:   vm.SubPath,
				})
			}
		}

		deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, resCtr)
	}

	if renderConfigTemplateVolume {
		deployment.Spec.Template.Annotations[common.ConfigTemplateVolumesAnnotationKey] = configTemplateVolumeName
		deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: configTemplateVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: partyKitInfo.configTemplatesCMName,
					},
				},
			},
		})
	}
	return deployment, nil
}

func buildPortNumberEnvs(allocatedPorts *appconfig.AllocatedPorts) []corev1.EnvVar {
	if allocatedPorts == nil {
		return nil
	}

	portNumberEnvs := make([]corev1.EnvVar, 0)
	for _, portInfo := range allocatedPorts.Ports {
		if portInfo == nil {
			continue
		}

		portNumberEnvs = append(portNumberEnvs, corev1.EnvVar{
			Name:  strings.ToUpper(strings.ReplaceAll(fmt.Sprintf(common.EnvPortNumber, portInfo.Name), "-", "_")),
			Value: strconv.Itoa(int(portInfo.Port)),
		})
	}
	return portNumberEnvs
}

func (c *Controller) updateDeployment(ctx context.Context, partyKitInfo *PartyKitInfo) error {
	deployment, err := c.deploymentLister.Deployments(partyKitInfo.domainID).Get(partyKitInfo.dkInfo.deploymentName)
	if err != nil {
		return fmt.Errorf("failed to check if need update deployment %v, %v", partyKitInfo.dkInfo.deploymentName, err)
	}

	deploymentCopy := deployment.DeepCopy()
	needUpdate := false
	for _, kdParty := range partyKitInfo.kd.Spec.Parties {
		if kdParty.DomainID == partyKitInfo.domainID && kdParty.Role == partyKitInfo.role {
			// check replicas
			if kdParty.Template.Replicas != nil && deploymentCopy.Spec.Replicas != nil && *kdParty.Template.Replicas != *deploymentCopy.Spec.Replicas {
				nlog.Debugf("Deployment %v/%v replicas changed from %v to %v", deploymentCopy.Namespace, deploymentCopy.Name, *deploymentCopy.Spec.Replicas, *kdParty.Template.Replicas)
				needUpdate = true
				deploymentCopy.Spec.Replicas = kdParty.Template.Replicas
			}

			// check strategy
			if kdParty.Template.Strategy != nil && !reflect.DeepEqual(*kdParty.Template.Strategy, deploymentCopy.Spec.Strategy) {
				nlog.Debugf("Deployment %v/%v strategy changed from %v to %v", deploymentCopy.Namespace, deploymentCopy.Name, deploymentCopy.Spec.Strategy, *kdParty.Template.Strategy)
				needUpdate = true
				deploymentCopy.Spec.Strategy = *kdParty.Template.Strategy
			}

			// check affinity
			// currently, only check when kd template affinity is not nil
			// allows manual modification of deployment affinity
			if kdParty.Template.Spec.Affinity != nil {
				affinity := kdParty.Template.Spec.Affinity.DeepCopy()
				buildAffinity(affinity, deploymentCopy.Name)

				if !reflect.DeepEqual(affinity, deploymentCopy.Spec.Template.Spec.Affinity) {
					needUpdate = true
					deploymentCopy.Spec.Template.Spec.Affinity = affinity
				}
			} else {
				// If affinity is not set in AppImage and original deployment Affinity is nil, add the default PodAntiAffinity
				if deploymentCopy.Spec.Template.Spec.Affinity == nil {
					needUpdate = true
					deploymentCopy.Spec.Template.Spec.Affinity = buildDefaultPodAntiAffinity(deploymentCopy.Name)
				}
			}

			// check container image
			for i, ctr := range deploymentCopy.Spec.Template.Spec.Containers {
				if ctr.Image != partyKitInfo.dkInfo.image {
					nlog.Debugf("Deployment %v/%v pod container %v image changed from %v to %v", deploymentCopy.Namespace, deploymentCopy.Name, ctr.Name, ctr.Image, partyKitInfo.dkInfo.image)
					needUpdate = true
					deploymentCopy.Spec.Template.Spec.Containers[i].Image = partyKitInfo.dkInfo.image
				}
			}

			// check container resources
			for _, pc := range partyKitInfo.deployTemplate.Spec.Containers {
				for i, dc := range deploymentCopy.Spec.Template.Spec.Containers {
					if pc.Name == dc.Name {
						if !reflect.DeepEqual(pc.Resources, dc.Resources) {
							nlog.Debugf("Deployment %v/%v pod container %v resources changed from %v to %v", deploymentCopy.Namespace, deploymentCopy.Name, pc.Name, dc.Resources.String(), pc.Resources.String())
							needUpdate = true
							deploymentCopy.Spec.Template.Spec.Containers[i].Resources = *pc.Resources.DeepCopy()
						}
					}
				}
			}

			// check input_config
			envExist := false
			for i, ctr := range deploymentCopy.Spec.Template.Spec.Containers {
				for j, ctrEnv := range ctr.Env {
					if ctrEnv.Name == common.EnvInputConfig {
						envExist = true
						if partyKitInfo.kd.Spec.InputConfig != ctrEnv.Value {
							nlog.Debugf("Deployment %v/%v pod container %v env %v changed from %v to %v", deploymentCopy.Namespace, deploymentCopy.Name, ctr.Name, common.EnvInputConfig, ctrEnv.Value, partyKitInfo.kd.Spec.InputConfig)
							needUpdate = true
							deploymentCopy.Spec.Template.Spec.Containers[i].Env[j].Value = partyKitInfo.kd.Spec.InputConfig
						}
					}
				}
			}
			if !envExist {
				nlog.Debugf("Deployment %v/%v pod containers need add env %v:%v", deploymentCopy.Namespace, deploymentCopy.Name, common.EnvInputConfig, partyKitInfo.kd.Spec.InputConfig)
				needUpdate = true
				for index := range deploymentCopy.Spec.Template.Spec.Containers {
					deploymentCopy.Spec.Template.Spec.Containers[index].Env = append(deploymentCopy.Spec.Template.Spec.Containers[index].Env, corev1.EnvVar{
						Name:  common.EnvInputConfig,
						Value: partyKitInfo.kd.Spec.InputConfig,
					})
				}
			}
		}
	}

	if needUpdate {
		_, err = c.kubeClient.AppsV1().Deployments(deploymentCopy.Namespace).Update(ctx, deploymentCopy, metav1.UpdateOptions{})
		if err != nil && !k8serrors.IsConflict(err) {
			return fmt.Errorf("failed to update deployment %v/%v, %v", deploymentCopy.Namespace, deployment.Name, err)
		}
	}

	return nil
}

func buildAffinity(affinity *corev1.Affinity, deploymentName string) {
	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"kuscia.secretflow/deployment-name": deploymentName},
	}

	if affinity.PodAntiAffinity != nil {
		for i := range affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[i].LabelSelector = labelSelector
		}
		for i := range affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution[i].PodAffinityTerm.LabelSelector = labelSelector
		}
	}

	if affinity.PodAffinity != nil {
		for i := range affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[i].LabelSelector = labelSelector
		}
		for i := range affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution[i].PodAffinityTerm.LabelSelector = labelSelector
		}
	}
}

func buildDefaultPodAntiAffinity(deploymentName string) *corev1.Affinity {

	labelSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"kuscia.secretflow/deployment-name": deploymentName},
	}
	affinity := &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: labelSelector,
						TopologyKey:   "kubernetes.io/hostname",
					},
				},
			},
		},
	}
	return affinity
}

func (c *Controller) selfParties(kd *kusciav1alpha1.KusciaDeployment) ([]kusciav1alpha1.KusciaDeploymentParty, error) {
	selfParties := make([]kusciav1alpha1.KusciaDeploymentParty, 0)
	for _, p := range kd.Spec.Parties {
		partyDomain, err := c.domainLister.Get(p.DomainID)
		if err != nil {
			return nil, err
		}
		if partyDomain.Spec.Role == "" {
			selfParties = append(selfParties, p)
		}
	}
	return selfParties, nil
}

func (c *Controller) isInitiatorController(kd *kusciav1alpha1.KusciaDeployment) (bool, error) {
	initiatorDomain, err := c.domainLister.Get(kd.Spec.Initiator)
	if err != nil {
		return false, err
	}
	if initiatorDomain.Spec.Role == "" {
		return true, nil
	}
	return false, nil
}
