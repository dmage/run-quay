package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/dmage/run-quay/pipeline"
	routev1 "github.com/openshift/api/route/v1"
	routeclientset "github.com/openshift/client-go/route/clientset/versioned"
	"github.com/ricardomaraschini/plumber"
	tektonv1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	tektonclientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/types"
)

// Compile time constants
var (
	Commit = "main"
)

var (
	addr       = flag.String("addr", ":8080", "The address to serve on")
	consoleURL = flag.String("console-url", "https://console.apps.test.gcp.quaydev.org", "The URL of the console")
)

var (
	//go:embed templates
	templatesContent embed.FS
	templates        = template.Must(template.ParseFS(templatesContent, "templates/*.html"))
)

var (
	errNoRoute = fmt.Errorf("no hostname found in route status")
)

// CreatePipelineObjects creates the objects required for a pipeline.
func CreatePipelineObjects(ctx context.Context, controllerClient client.Client, namespace string) error {
	// sets up some "mutators" so we can intercept the objects before they are created in
	// the api. we can also mutate the kustomization.yaml file here.
	opts := []plumber.Option{
		plumber.WithKustomizeMutator(
			func(_ context.Context, k *types.Kustomization) error {
				// here we mutate the kustomization.yaml file
				// present inside the directory we are
				// rendering. Makes sense to change the
				// namespace globally (i.e. here) to all
				// objects instead of in an object by object
				// manner (through an ObjectMutator below).
				k.Namespace = namespace
				return nil
			},
		),
		plumber.WithObjectMutator(
			func(ctx context.Context, obj client.Object) error {
				// here we can mutate the objects before they
				// are created or patched. Any change is
				// possible.
				return nil
			},
		),
	}

	creator := plumber.NewRenderer(controllerClient, pipeline.FS, opts...)
	if err := creator.Render(ctx, "base"); err != nil {
		return fmt.Errorf("error rendering pipeline: %w", err)
	}
	return nil
}

// GetRouteHostname returns the hostname of the registry route in the given namespace.
func GetRouteHostname(ctx context.Context, routeClient routeclientset.Interface, namespace string) (string, error) {
	route, err := routeClient.RouteV1().Routes(namespace).Get(ctx, "registry", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("error getting route: %w", err)
	}
	for _, ingress := range route.Status.Ingress {
		if ingress.Host != "" {
			return ingress.Host, nil
		}
	}
	return "", errNoRoute
}

// CreateNamespace creates a new namespace for a Quay instance.
func CreateNamespace(ctx context.Context, kubeClient kubernetes.Interface, tektonClient tektonclientset.Interface, routeClient routeclientset.Interface, controllerClient client.Client, prNumber int) (string, error) {
	namespaceName := fmt.Sprintf("quay-%d-%s", prNumber, rand.String(5))
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
			Annotations: map[string]string{
				"run-quay/pr-number": strconv.Itoa(prNumber),
			},
			Labels: map[string]string{
				"run-quay": "true",
			},
		},
	}
	_, err := kubeClient.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("error creating namespace: %w", err)
	}

	err = CreatePipelineObjects(ctx, controllerClient, namespaceName)
	if err != nil {
		return "", fmt.Errorf("error creating pipeline objects: %w", err)
	}

	_, err = tektonClient.TektonV1beta1().PipelineRuns(namespaceName).Create(ctx, &tektonv1beta1.PipelineRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "build",
		},
		Spec: tektonv1beta1.PipelineRunSpec{
			Params: []tektonv1beta1.Param{
				{
					Name: "git-url",
					Value: tektonv1beta1.ArrayOrString{
						Type:      tektonv1beta1.ParamTypeString,
						StringVal: "https://github.com/quay/quay",
					},
				},
				{
					Name: "git-revision",
					Value: tektonv1beta1.ArrayOrString{
						Type:      tektonv1beta1.ParamTypeString,
						StringVal: fmt.Sprintf("refs/pull/%d/head", prNumber),
					},
				},
				{
					Name: "image",
					Value: tektonv1beta1.ArrayOrString{
						Type:      tektonv1beta1.ParamTypeString,
						StringVal: fmt.Sprintf("image-registry.openshift-image-registry.svc:5000/%s/quay", namespaceName),
					},
				},
				{
					Name: "namespace",
					Value: tektonv1beta1.ArrayOrString{
						Type:      tektonv1beta1.ParamTypeString,
						StringVal: namespaceName,
					},
				},
				{
					Name: "hostname",
					Value: tektonv1beta1.ArrayOrString{
						Type:      tektonv1beta1.ParamTypeString,
						StringVal: fmt.Sprintf("registry-%s.apps.test.gcp.quaydev.org", namespaceName), // FIXME: use the hostname from the route
					},
				},
			},
			PipelineRef: &tektonv1beta1.PipelineRef{
				Name: "build",
			},
			ServiceAccountName: "pipeline",
			Timeout:            &metav1.Duration{Duration: 1 * time.Hour},
			Workspaces: []tektonv1beta1.WorkspaceBinding{
				{
					Name: "shared-workspace",
					VolumeClaimTemplate: &corev1.PersistentVolumeClaim{
						Spec: corev1.PersistentVolumeClaimSpec{
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteOnce,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceStorage: resource.MustParse("500Mi"),
								},
							},
						},
					},
				},
				{
					Name: "manifests",
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "manifests",
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("error creating pipeline run: %w", err)
	}

	return namespaceName, nil
}

// DeleteOldNamespaces deletes namespaces with the label run-quay=true that are
// older than 12 hours.
func DeleteOldNamespaces(ctx context.Context, kubeClient kubernetes.Interface) error {
	namespaces, err := kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: "run-quay=true",
	})
	if err != nil {
		return fmt.Errorf("error listing namespaces: %w", err)
	}
	var errs []error
	for _, namespace := range namespaces.Items {
		age := time.Since(namespace.CreationTimestamp.Time)
		if age > 12*time.Hour {
			klog.Infof("deleting old namespace %s (%s old)", namespace.Name, age.Round(time.Second))
			err := kubeClient.CoreV1().Namespaces().Delete(ctx, namespace.Name, metav1.DeleteOptions{})
			if err != nil {
				errs = append(errs, fmt.Errorf("error deleting namespace %s: %w", namespace.Name, err))
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	ctx := context.Background()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		klog.Exitf("error adding corev1 to scheme: %w", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		klog.Exitf("error adding appsv1 to scheme: %w", err)
	}
	if err := tektonv1beta1.AddToScheme(scheme); err != nil {
		klog.Exitf("error adding tektonv1beta1 to scheme: %w", err)
	}
	if err := routev1.AddToScheme(scheme); err != nil {
		klog.Exitf("error adding routev1 to scheme: %w", err)
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	configOverrides := &clientcmd.ConfigOverrides{}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		klog.Exitf("error creating client config: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Exitf("error creating client: %v", err)
	}

	tektonClient, err := tektonclientset.NewForConfig(config)
	if err != nil {
		klog.Exitf("error creating tekton client: %v", err)
	}

	routeClient, err := routeclientset.NewForConfig(config)
	if err != nil {
		klog.Exitf("error creating route client: %v", err)
	}

	controllerClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		klog.Exitf("error creating controller client: %v", err)
	}

	go func() {
		for {
			err := DeleteOldNamespaces(ctx, kubeClient)
			if err != nil {
				klog.Errorf("error deleting old namespaces: %v", err)
			}
			time.Sleep(10 * time.Minute)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				r.Header.Set("Content-Type", "text/html")
				err := templates.ExecuteTemplate(w, "main.html", struct {
					Commit string
				}{
					Commit: Commit,
				})
				if err != nil {
					klog.Errorf("error executing template: %v", err)
				}
			case http.MethodPost:
				prNumber, err := strconv.Atoi(r.FormValue("pr"))
				if err != nil {
					http.Error(w, "invalid PR number", http.StatusBadRequest)
					return
				}
				namespaceName, err := CreateNamespace(ctx, kubeClient, tektonClient, routeClient, controllerClient, prNumber)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				http.Redirect(w, r, fmt.Sprintf("/%s", namespaceName), http.StatusSeeOther)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		default:
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				namespaceName := r.URL.Path[1:]
				namespace, err := kubeClient.CoreV1().Namespaces().Get(ctx, namespaceName, metav1.GetOptions{})
				if errors.IsNotFound(err) {
					http.Error(w, "namespace not found", http.StatusNotFound)
					return
				} else if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}

				pipelineRun, err := tektonClient.TektonV1beta1().PipelineRuns(namespace.Name).Get(ctx, "build", metav1.GetOptions{})
				if errors.IsNotFound(err) {
					http.Error(w, "pipeline run not found", http.StatusNotFound)
					return
				} else if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}

				succeeded := corev1.ConditionUnknown
				cond := pipelineRun.Status.GetCondition(apis.ConditionSucceeded)
				if cond != nil {
					succeeded = cond.Status
				}

				var estimatedCompletionTime time.Duration
				if succeeded == corev1.ConditionUnknown {
					estimatedCompletionTime = namespace.CreationTimestamp.Time.Add(540 * time.Second).Sub(time.Now())
				}

				r.Header.Set("Content-Type", "text/html")
				err = templates.ExecuteTemplate(w, "namespace.html", struct {
					ConsoleURL              string
					Namespace               *corev1.Namespace
					PipelineRun             *tektonv1beta1.PipelineRun
					Succeeded               corev1.ConditionStatus
					EstimatedCompletionTime time.Duration
				}{
					ConsoleURL:              *consoleURL,
					Namespace:               namespace,
					PipelineRun:             pipelineRun,
					Succeeded:               succeeded,
					EstimatedCompletionTime: estimatedCompletionTime.Round(10 * time.Second),
				})
				if err != nil {
					klog.Errorf("error executing template: %v", err)
				}
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		}
	})
	if err := http.ListenAndServe(*addr, nil); err != nil {
		klog.Exitf("error listening: %v", err)
	}
}
