/*

 Copyright 2021 The KubeVela Authors.

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

package componentdefinition

import (
	"context"
	"fmt"

	cpv1alpha1 "github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	oamctrl "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	coredef "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1alpha2/core"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
	"github.com/oam-dev/kubevela/pkg/dsl/definition"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	"github.com/oam-dev/kubevela/pkg/oam/util"
)

// Reconciler reconciles a ComponentDefinition object
type Reconciler struct {
	client.Client
	dm                   discoverymapper.DiscoveryMapper
	pd                   *definition.PackageDiscover
	Scheme               *runtime.Scheme
	record               event.Recorder
	defRevLimit          int
	concurrentReconciles int
}

// Reconcile is the main logic for ComponentDefinition controller
func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	klog.InfoS("Reconcile componentDefinition", "componentDefinition", klog.KRef(req.Namespace, req.Name))
	ctx := context.Background()

	var componentDefinition v1beta1.ComponentDefinition
	if err := r.Get(ctx, req.NamespacedName, &componentDefinition); err != nil {
		if apierrors.IsNotFound(err) {
			err = nil
		}
		return ctrl.Result{}, err
	}

	// this is a placeholder for finalizer here in the future
	if componentDefinition.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	handler := handler{
		Client: r.Client,
		dm:     r.dm,
		cd:     &componentDefinition,
	}

	// refresh package discover when componentDefinition is registered
	if handler.cd.Spec.Workload.Type == "" {
		err := utils.RefreshPackageDiscover(r.dm, r.pd, handler.cd.Spec.Workload.Definition,
			common.DefinitionReference{}, types.TypeComponentDefinition)
		if err != nil {
			klog.ErrorS(err, "cannot discover the open api of the CRD")
			r.record.Event(&componentDefinition, event.Warning("cannot discover the open api of the CRD", err))
			return ctrl.Result{}, util.PatchCondition(ctx, r, &componentDefinition,
				cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrRefreshPackageDiscover, err)))
		}
	}

	// generate DefinitionRevision from componentDefinition
	defRev, isNewRevision, err := coredef.GenerateDefinitionRevision(ctx, r.Client, &componentDefinition)
	if err != nil {
		klog.InfoS("Could not generate DefinitionRevision", "componentDefinition", klog.KObj(&componentDefinition), "err", err)
		r.record.Event(&componentDefinition, event.Warning("Could not generate DefinitionRevision", err))
		return ctrl.Result{}, util.PatchCondition(ctx, r, &componentDefinition,
			cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrGenerateDefinitionRevision, componentDefinition.Name, err)))
	}

	if !isNewRevision {
		if err = r.createOrUpdateComponentDefRevision(ctx, req.Namespace, &componentDefinition, defRev); err != nil {
			klog.InfoS("Could not update DefinitionRevision", "err", err)
			r.record.Event(&(componentDefinition), event.Warning("Could not update DefinitionRevision", err))
			return ctrl.Result{}, util.PatchCondition(ctx, r, &(componentDefinition),
				cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrCreateOrUpdateDefinitionRevision, defRev.Name, err)))
		}
		klog.InfoS("Successfully update definitionRevision", "definitionRevision", klog.KObj(defRev))

		if err := coredef.CleanUpDefinitionRevision(ctx, r.Client, &componentDefinition, r.defRevLimit); err != nil {
			klog.Error("[Garbage collection]")
			r.record.Event(&componentDefinition, event.Warning("failed to garbage collect DefinitionRevision of type ComponentDefinition", err))
		}
		return ctrl.Result{}, nil
	}

	workloadType, err := handler.CreateWorkloadDefinition(ctx)
	if err != nil {
		klog.ErrorS(err, "cannot create converted WorkloadDefinition")
		r.record.Event(&componentDefinition, event.Warning("cannot store capability in ConfigMap", err))
		return ctrl.Result{}, util.PatchCondition(ctx, r, &componentDefinition,
			cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrCreateConvertedWorklaodDefinition, componentDefinition.Name, err)))
	}
	klog.InfoS("Successfully create WorkloadDefinition", "name", componentDefinition.Name)

	var def utils.CapabilityComponentDefinition
	def.Name = req.NamespacedName.Name
	def.WorkloadType = workloadType
	def.ComponentDefinition = componentDefinition
	switch workloadType {
	case util.ReferWorkload:
		def.WorkloadDefName = componentDefinition.Spec.Workload.Type
	case util.HELMDef:
		def.Helm = componentDefinition.Spec.Schematic.HELM
	case util.KubeDef:
		def.Kube = componentDefinition.Spec.Schematic.KUBE
	case util.TerraformDef:
		def.Terraform = componentDefinition.Spec.Schematic.Terraform
	default:
	}

	// Store the parameter of componentDefinition to configMap
	err = def.StoreOpenAPISchema(ctx, r.Client, r.pd, req.Namespace, req.Name, defRev.Name)
	if err != nil {
		klog.ErrorS(err, "cannot store capability in ConfigMap")
		r.record.Event(&(def.ComponentDefinition), event.Warning("cannot store capability in ConfigMap", err))
		return ctrl.Result{}, util.PatchCondition(ctx, r, &(def.ComponentDefinition),
			cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrStoreCapabilityInConfigMap, def.Name, err)))
	}
	klog.Info("Successfully stored Capability Schema in ConfigMap")

	if err = r.createOrUpdateComponentDefRevision(ctx, req.Namespace, &def.ComponentDefinition, defRev); err != nil {
		klog.ErrorS(err, "cannot create DefinitionRevision")
		r.record.Event(&(def.ComponentDefinition), event.Warning("cannot create DefinitionRevision", err))
		return ctrl.Result{}, util.PatchCondition(ctx, r, &(def.ComponentDefinition),
			cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrCreateOrUpdateDefinitionRevision, defRev.Name, err)))
	}
	klog.InfoS("Successfully create definitionRevision", "definitionRevision", klog.KObj(defRev))

	def.ComponentDefinition.Status.LatestRevision = &common.Revision{
		Name:         defRev.Name,
		Revision:     defRev.Spec.Revision,
		RevisionHash: defRev.Spec.RevisionHash,
	}

	if err := r.UpdateStatus(ctx, &def.ComponentDefinition); err != nil {
		klog.ErrorS(err, "cannot update componentDefinition Status")
		r.record.Event(&(def.ComponentDefinition), event.Warning("cannot update ComponentDefinition Status", err))
		return ctrl.Result{}, util.PatchCondition(ctx, r, &(def.ComponentDefinition),
			cpv1alpha1.ReconcileError(fmt.Errorf(util.ErrUpdateComponentDefinition, def.ComponentDefinition.Name, err)))
	}

	if err := coredef.CleanUpDefinitionRevision(ctx, r.Client, &def.ComponentDefinition, r.defRevLimit); err != nil {
		klog.Error("[Garbage collection]")
		r.record.Event(&def.ComponentDefinition, event.Warning("failed to garbage collect DefinitionRevision of type ComponentDefinition", err))
	}

	return ctrl.Result{}, nil
}

func (r *Reconciler) createOrUpdateComponentDefRevision(ctx context.Context, namespace string,
	componentDef *v1beta1.ComponentDefinition, defRev *v1beta1.DefinitionRevision) error {

	ownerReference := []metav1.OwnerReference{{
		APIVersion:         componentDef.APIVersion,
		Kind:               componentDef.Kind,
		Name:               componentDef.Name,
		UID:                componentDef.GetUID(),
		Controller:         pointer.BoolPtr(true),
		BlockOwnerDeletion: pointer.BoolPtr(true),
	}}

	defRev.SetLabels(componentDef.GetLabels())
	defRev.SetLabels(util.MergeMapOverrideWithDst(defRev.Labels,
		map[string]string{oam.LabelComponentDefinitionName: componentDef.Name}))
	defRev.SetNamespace(namespace)
	defRev.SetAnnotations(componentDef.GetAnnotations())
	defRev.SetOwnerReferences(ownerReference)

	rev := &v1beta1.DefinitionRevision{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: defRev.Name}, rev); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, defRev)
		}
		return err
	}

	rev.SetAnnotations(defRev.GetAnnotations())
	rev.SetLabels(defRev.GetLabels())
	rev.SetOwnerReferences(ownerReference)
	return r.Update(ctx, rev)
}

// UpdateStatus updates v1beta1.ComponentDefinition's Status with retry.RetryOnConflict
func (r *Reconciler) UpdateStatus(ctx context.Context, def *v1beta1.ComponentDefinition, opts ...client.UpdateOption) error {
	status := def.DeepCopy().Status
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		if err = r.Get(ctx, client.ObjectKey{Namespace: def.Namespace, Name: def.Name}, def); err != nil {
			return
		}
		def.Status = status
		return r.Status().Update(ctx, def, opts...)
	})
}

// SetupWithManager will setup with event recorder
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.record = event.NewAPIRecorder(mgr.GetEventRecorderFor("ComponentDefinition")).
		WithAnnotations("controller", "ComponentDefinition")
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.concurrentReconciles,
		}).
		For(&v1beta1.ComponentDefinition{}).
		Complete(r)
}

// Setup adds a controller that reconciles ComponentDefinition.
func Setup(mgr ctrl.Manager, args oamctrl.Args) error {
	r := Reconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		dm:                   args.DiscoveryMapper,
		pd:                   args.PackageDiscover,
		defRevLimit:          args.DefRevisionLimit,
		concurrentReconciles: args.ConcurrentReconciles,
	}
	return r.SetupWithManager(mgr)
}
