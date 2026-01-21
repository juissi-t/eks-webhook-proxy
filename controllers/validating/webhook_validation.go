package validating

import (
	"context"
	"errors"
	"github.com/go-logr/logr"
	"github.com/CharlieR-o-o-t/eks-webhook-proxy/pkg/proxy"
	"github.com/CharlieR-o-o-t/eks-webhook-proxy/pkg/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/CharlieR-o-o-t/eks-webhook-proxy/config"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ControllerName = "validating-controller"
)

type Controller struct {
	Config *config.Config
	Client client.Client
	Proxy  *proxy.Proxy
	Log    logr.Logger
}

func New(config *config.Config, client client.Client, proxy *proxy.Proxy) *Controller {
	return &Controller{
		Config: config,
		Client: client,
		Log:    log.Log.WithName("validating-controller"),
	}
}
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := c.Log.WithValues("name", req.String())

	var webhookServiceMap = make(map[*admissionv1.ServiceReference]struct{})

	webhookObj := new(admissionv1.ValidatingWebhookConfiguration)
	if err := c.Client.Get(ctx, types.NamespacedName{Name: req.Name}, webhookObj); err != nil {
		// webhook deleted, do nothing.
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		c.Log.Error(err, "unable to fetch validation webhook")
		return reconcile.Result{}, err
	}

	// Need to create only one nodeProxy service per webhook.
	for _, webhook := range webhookObj.Webhooks {
		if webhook.ClientConfig.Service == nil {
			// Probably webhook url is used, no need for proxy.
			continue
		}

		serviceRef := webhook.ClientConfig.Service
		if serviceRef.Port == nil {
			serviceRef.Port = ptr.To(utils.DefaultWebhookPort)
		}
		webhookServiceMap[serviceRef] = struct{}{}
	}

	for webhookServiceRef := range webhookServiceMap {
		serviceKey := types.NamespacedName{Name: webhookServiceRef.Name, Namespace: webhookServiceRef.Namespace}
		log := log.WithValues("service", serviceKey)

		serviceProxy, err := c.Proxy.EnsureServiceProxy(ctx, webhookServiceRef)
		if err != nil {
			if errors.Is(err, proxy.ErrServiceNotFound) {
				log.V(5).Info("webhook service not found, skipping")
				continue
			}
			log.Error(err, "unable to create Proxy")
			return reconcile.Result{}, err
		}
		if err := c.Proxy.EnsureProxyEndpointSlices(ctx, serviceProxy); err != nil {
			log.Error(err, "unable to create proxy EndpointSlices")
			return reconcile.Result{}, err
		}

		if err := c.Proxy.UnbindPodEndpoints(ctx, webhookServiceRef); err != nil {
			log.Error(err, "unable to unbind Pod Endpoints from webhook service")
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {

	// Contains webhook service.
	predicateValidating := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		webhook := obj.(*admissionv1.ValidatingWebhookConfiguration)

		if len(webhook.Webhooks) == 0 {
			return false
		}

		for _, validating := range webhook.Webhooks {
			if validating.ClientConfig.Service != nil {
				return true
			}
		}
		return false
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named(ControllerName).
		Watches(
			&admissionv1.ValidatingWebhookConfiguration{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(predicateValidating),
		).
		Complete(c)
}
