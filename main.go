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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/types"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/ricardomaraschini/plumber"
	tektonv1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"knative.dev/pkg/apis"

	"github.com/dmage/run-quay/pipeline"
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

// CreatePipeline creates the objects required for a pipeline.
func CreatePipeline(ctx context.Context, cli client.Client, ns string, prnr int) error {
	opts := []plumber.Option{
		plumber.WithKustomizeMutator(
			namespaceSetter(ns),
		),
		plumber.WithObjectMutator(
			namespaceMutator(ns, prnr),
		),
		plumber.WithObjectMutator(
			pipelineRunMutator(ns, prnr),
		),
	}

	creator := plumber.NewRenderer(cli, pipeline.FS, opts...)
	if err := creator.Render(ctx, "base"); err != nil {
		return fmt.Errorf("error rendering pipeline: %w", err)
	}

	if err := creator.Render(ctx, "pipelinerun"); err != nil {
		return fmt.Errorf("error rendering pipelinerun: %w", err)
	}
	return nil
}

// namespaceSetter returns a kustomizes mutator that sets the default namespace in a Kustomization
// struct passed in as argument.
func namespaceSetter(ns string) plumber.KustomizeMutator {
	return func(_ context.Context, k *types.Kustomization) error {
		k.Namespace = ns
		return nil
	}
}

// namespaceMutator mutates the namespace created, appends to it the pull request number as an
// annotation.
func namespaceMutator(nsname string, prnr int) plumber.ObjectMutator {
	return func(_ context.Context, obj client.Object) error {
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			return nil
		}

		ns.Name = nsname
		ns.Annotations = map[string]string{"run-quay/pr-number": strconv.Itoa(prnr)}
		return nil
	}
}

// pipelineRunMutator mutates the created tekton pipeline run object. this function customizes
// the params passed on to the pipeline run.
func pipelineRunMutator(ns string, prnr int) plumber.ObjectMutator {
	return func(_ context.Context, obj client.Object) error {
		piperun, ok := obj.(*tektonv1beta1.PipelineRun)
		if !ok {
			return nil
		}

		img := fmt.Sprintf("image-registry.openshift-image-registry.svc:5000/%s/quay", ns)
		hostname := fmt.Sprintf("registry-%s.apps.test.gcp.quaydev.org", ns)

		params := []tektonv1beta1.Param{
			{
				Name: "git-revision",
				Value: tektonv1beta1.ArrayOrString{
					Type:      tektonv1beta1.ParamTypeString,
					StringVal: fmt.Sprintf("refs/pull/%d/head", prnr),
				},
			},
			{
				Name: "image",
				Value: tektonv1beta1.ArrayOrString{
					Type:      tektonv1beta1.ParamTypeString,
					StringVal: img,
				},
			},
			{
				Name: "namespace",
				Value: tektonv1beta1.ArrayOrString{
					Type:      tektonv1beta1.ParamTypeString,
					StringVal: ns,
				},
			},
			{
				Name: "hostname",
				Value: tektonv1beta1.ArrayOrString{
					Type:      tektonv1beta1.ParamTypeString,
					StringVal: hostname,
				},
			},
		}

		piperun.Spec.Params = append(piperun.Spec.Params, params...)
		return nil
	}
}

// DeleteOldNamespaces deletes namespaces with the label run-quay=true that are
// older than 12 hours.
func DeleteOldNamespaces(ctx context.Context, cli client.Client) error {
	opts := &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(
			map[string]string{"run-quay": "true"},
		),
	}

	var nslist corev1.NamespaceList
	if err := cli.List(ctx, &nslist, opts); err != nil {
		return fmt.Errorf("error listing namespaces: %w", err)
	}

	var errs []error
	for _, namespace := range nslist.Items {
		age := time.Since(namespace.CreationTimestamp.Time)
		if age < 12*time.Hour {
			continue
		}

		if err := cli.Delete(ctx, &namespace); err != nil {
			errs = append(errs, err)
		}

		klog.Infof("deleted namespace %s (%s old)", namespace.Name, age.Round(time.Second))
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

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		klog.Exitf("error creating client config: %v", err)
	}

	cli, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		klog.Exitf("error creating controller client: %v", err)
	}

	go func() {
		for {
			err := DeleteOldNamespaces(ctx, cli)
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

				ns := fmt.Sprintf("quay-%d-%s", prNumber, rand.String(5))
				if err := CreatePipeline(ctx, cli, ns, prNumber); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				http.Redirect(w, r, fmt.Sprintf("/%s", ns), http.StatusSeeOther)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		default:
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				namespaceName := r.URL.Path[1:]
				nsn := ktypes.NamespacedName{Name: namespaceName}
				var namespace corev1.Namespace
				err := cli.Get(ctx, nsn, &namespace)
				if errors.IsNotFound(err) {
					http.Error(w, "namespace not found", http.StatusNotFound)
					return
				} else if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}

				nsn = ktypes.NamespacedName{
					Namespace: namespace.Name,
					Name:      "build",
				}

				var pipelineRun tektonv1beta1.PipelineRun
				if err = cli.Get(ctx, nsn, &pipelineRun); errors.IsNotFound(err) {
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
					Namespace:               &namespace,
					PipelineRun:             &pipelineRun,
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
