package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	tektonv1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	tektonclientset "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"knative.dev/pkg/apis"
)

// Compile time constants
var (
	Commit = "main"
)

var (
	addr = flag.String("addr", ":8080", "The address to serve on")
)

var (
	//go:embed templates
	templatesContent embed.FS
	templates        = template.Must(template.ParseFS(templatesContent, "templates/*.html"))
)

// CreatePipelineObjects runs a script to create the objects required for a pipeline.
func CreatePipelineObjects(ctx context.Context, namespace string) error {
	cmd := exec.Command("./create-pipeline.sh", namespace)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error running script: %w", err)
	}
	return nil
}

// CreateNamespace creates a new namespace for a Quay instance.
func CreateNamespace(ctx context.Context, kubeClient kubernetes.Interface, tektonClient tektonclientset.Interface, prNumber int) (string, error) {
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

	err = CreatePipelineObjects(ctx, namespaceName)
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
				namespaceName, err := CreateNamespace(ctx, kubeClient, tektonClient, prNumber)
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
					Namespace               *corev1.Namespace
					PipelineRun             *tektonv1beta1.PipelineRun
					Succeeded               corev1.ConditionStatus
					EstimatedCompletionTime time.Duration
				}{
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
