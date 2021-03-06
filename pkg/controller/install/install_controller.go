package install

import (
	"context"
	"flag"
	"fmt"

	mf "github.com/jcrossley3/manifestival"
	servingv1alpha1 "github.com/openshift-knative/knative-serving-operator/pkg/apis/serving/v1alpha1"
	"github.com/openshift-knative/knative-serving-operator/version"

	"github.com/operator-framework/operator-sdk/pkg/predicate"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	filename = flag.String("filename", "deploy/resources",
		"The filename containing the YAML resources to apply")
	recursive = flag.Bool("recursive", false,
		"If filename is a directory, process all manifests recursively")
	installNs = flag.String("install-ns", "",
		"The namespace in which to create an Install resource, if none exist")
	log = logf.Log.WithName("controller_install")
	// Platform-specific configuration via manifestival transformations
	platformFuncs []func(client.Client, *runtime.Scheme) []mf.Transformer
)

// Add creates a new Install Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	manifest, err := mf.NewManifest(*filename, *recursive, mgr.GetClient())
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr, manifest))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, man mf.Manifest) reconcile.Reconciler {
	return &ReconcileInstall{client: mgr.GetClient(), scheme: mgr.GetScheme(), config: man}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("install-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Install
	err = c.Watch(&source.Kind{Type: &servingv1alpha1.Install{}}, &handler.EnqueueRequestForObject{}, predicate.GenerationChangedPredicate{})
	if err != nil {
		return err
	}

	// Make an attempt to create an Install CR, if necessary
	if len(*installNs) > 0 {
		c, _ := client.New(mgr.GetConfig(), client.Options{})
		go autoInstall(c, *installNs)
	}
	return nil
}

var _ reconcile.Reconciler = &ReconcileInstall{}

// ReconcileInstall reconciles a Install object
type ReconcileInstall struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	config mf.Manifest
}

// Reconcile reads that state of the cluster for a Install object and makes changes based on the state read
// and what is in the Install.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileInstall) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Install")

	// Fetch the Install instance
	instance := &servingv1alpha1.Install{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			r.config.DeleteAll()
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	stages := []func(*servingv1alpha1.Install) error{
		r.transform,
		r.install,
		r.deleteObsoleteResources,
		r.configure, // TODO: move to transform?
	}

	for _, stage := range stages {
		if err := stage(instance); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// Transform resources as appropriate for the spec and platform
func (r *ReconcileInstall) transform(instance *servingv1alpha1.Install) error {
	fns := []mf.Transformer{mf.InjectOwner(instance)}
	if len(instance.Spec.Namespace) > 0 {
		fns = append(fns, mf.InjectNamespace(instance.Spec.Namespace))
	}
	for _, f := range platformFuncs {
		fns = append(fns, f(r.client, r.scheme)...)
	}
	r.config.Transform(fns...)
	return nil
}

// Apply the embedded resources
func (r *ReconcileInstall) install(instance *servingv1alpha1.Install) error {
	if err := r.config.ApplyAll(); err != nil {
		return err
	}
	// Update status
	instance.Status.Resources = r.config.Resources
	instance.Status.Version = version.Version
	if err := r.client.Status().Update(context.TODO(), instance); err != nil {
		return err
	}
	return nil
}

// Set ConfigMap values from Install spec
func (r *ReconcileInstall) configure(instance *servingv1alpha1.Install) error {
	for suffix, config := range instance.Spec.Config {
		name := "config-" + suffix
		cm, err := r.config.Get(r.config.Find("v1", "ConfigMap", name))
		if err != nil {
			return err
		}
		if cm == nil {
			log.Error(fmt.Errorf("ConfigMap '%s' not found", name), "Invalid Install spec")
			continue
		}
		if err := r.updateConfigMap(cm, config); err != nil {
			return err
		}
	}
	return nil
}

// Set some data in a configmap, only overwriting common keys
func (r *ReconcileInstall) updateConfigMap(cm *unstructured.Unstructured, data map[string]string) error {
	for k, v := range data {
		message := []interface{}{"map", cm.GetName(), k, v}
		if x, found, _ := unstructured.NestedString(cm.Object, "data", k); found {
			if v == x {
				continue
			}
			message = append(message, "previous", x)
		}
		log.Info("Setting", message...)
		unstructured.SetNestedField(cm.Object, v, "data", k)
	}
	return r.config.Apply(cm)
}

// Delete obsolete istio-system resources, if any
func (r *ReconcileInstall) deleteObsoleteResources(instance *servingv1alpha1.Install) error {
	resource := &unstructured.Unstructured{}
	resource.SetNamespace("istio-system")
	resource.SetName("knative-ingressgateway")
	resource.SetAPIVersion("v1")
	resource.SetKind("Service")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("apps/v1")
	resource.SetKind("Deployment")
	if err := r.config.Delete(resource); err != nil {
		return err
	}
	resource.SetAPIVersion("autoscaling/v1")
	resource.SetKind("HorizontalPodAutoscaler")
	return r.config.Delete(resource)
}

// This may or may not be a good idea
func autoInstall(c client.Client, ns string) (err error) {
	const path = "deploy/crds/serving_v1alpha1_install_cr.yaml"
	log.Info("Automatic Install requested", "namespace", ns)
	installList := &servingv1alpha1.InstallList{}
	err = c.List(context.TODO(), &client.ListOptions{Namespace: ns}, installList)
	if err != nil {
		log.Error(err, "Unable to list Installs")
		return err
	}
	if len(installList.Items) == 0 {
		if manifest, err := mf.NewManifest(path, false, c); err == nil {
			if err = manifest.Transform(mf.InjectNamespace(ns)).ApplyAll(); err != nil {
				log.Error(err, "Unable to create Install")
			}
		} else {
			log.Error(err, "Unable to create Install manifest")
		}
	} else {
		log.Info("Install found", "name", installList.Items[0].Name)
	}
	return err
}
