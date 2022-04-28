package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"regexp"
	"strings"
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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"knative.dev/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"
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

	//go:embed config.yaml
	configContent embed.FS
)

var (
	errNoRoute = fmt.Errorf("no hostname found in route status")
)

var (
	githubRepoRegExp        = regexp.MustCompile(`^(?:https?://)github.com/([a-z0-9-]+)/([a-z0-9-]+)$`)
	githubPullRequestRegExp = regexp.MustCompile(`^(?:https?://)github.com/([a-z0-9-]+)/([a-z0-9-]+)/pull/([0-9]+)(?:/[a-z]+)?$`)
	githubBranchRegExp      = regexp.MustCompile(`^(?:https?://)github.com/([a-z0-9-]+)/([a-z0-9-]+)/tree/([a-z0-9._-]+)$`)
	githubTagRegExp         = regexp.MustCompile(`^(?:https?://)github.com/([a-z0-9-]+)/([a-z0-9-]+)/releases/tag/([a-z0-9._-]+)$`)
	numberRegExp            = regexp.MustCompile(`^[0-9]+$`)
	nonAlphaNumericRegExp   = regexp.MustCompile(`[^a-zA-Z0-9]+`)
	multipleHyphensRegExp   = regexp.MustCompile(`--+`)
)

// ParseSource converts a pull request URL into a git refernce.
func ParseSource(source string) (string, error) {
	if match := githubRepoRegExp.FindStringSubmatch(source); match != nil {
		if match[1] != "quay" || match[2] != "quay" {
			return "", fmt.Errorf("unsupported repository %s/%s", match[1], match[2])
		}
		return "HEAD", nil
	}
	if match := githubPullRequestRegExp.FindStringSubmatch(source); match != nil {
		if match[1] != "quay" || match[2] != "quay" {
			return "", fmt.Errorf("unsupported repository %s/%s", match[1], match[2])
		}
		return fmt.Sprintf("refs/pull/%s/head", match[3]), nil
	}
	if match := githubBranchRegExp.FindStringSubmatch(source); match != nil {
		if match[1] != "quay" || match[2] != "quay" {
			return "", fmt.Errorf("unsupported repository %s/%s", match[1], match[2])
		}
		return fmt.Sprintf("refs/heads/%s", match[3]), nil
	}
	if match := githubTagRegExp.FindStringSubmatch(source); match != nil {
		if match[1] != "quay" || match[2] != "quay" {
			return "", fmt.Errorf("unsupported repository %s/%s", match[1], match[2])
		}
		return fmt.Sprintf("refs/tags/%s", match[3]), nil
	}
	if numberRegExp.MatchString(source) {
		return fmt.Sprintf("refs/pull/%s/head", source), nil
	}
	return "", fmt.Errorf("unsupported source %s", source)
}

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
	if errors.IsNotFound(err) {
		return "", errNoRoute
	}
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

// RenderConfig generates the config.yaml file.
func RenderConfig(namespace string, hostname string, overrides map[string]interface{}) (string, error) {
	f, err := configContent.Open("config.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}

	config := map[string]interface{}{
		"DB_URI": fmt.Sprintf("postgresql://quay:quay@quay-postgresql.%s.svc/quay", namespace),
		"BUILDLOGS_REDIS": map[string]interface{}{
			"host": fmt.Sprintf("quay-redis.%s.svc", namespace),
			"port": 6379,
		},
		"USER_EVENTS_REDIS": map[string]interface{}{
			"host": fmt.Sprintf("quay-redis.%s.svc", namespace),
			"port": 6379,
		},
		"SERVER_HOSTNAME": hostname,
	}
	if err := yaml.Unmarshal(buf, &config); err != nil {
		return "", err
	}

	for k, v := range overrides {
		config[k] = v
	}

	result, err := yaml.Marshal(config)
	if err != nil {
		return "", err
	}

	return string(result), nil
}

// GitRefSlug returns the slug for a git refernce.
func GitRefSlug(gitRef string) (string, error) {
	var slug string
	if strings.HasPrefix(gitRef, "refs/pull/") && strings.HasSuffix(gitRef, "/head") {
		slug = strings.TrimSuffix(strings.TrimPrefix(gitRef, "refs/pull/"), "/head")
	} else {
		parts := strings.Split(gitRef, "/")
		slug = parts[len(parts)-1]
	}
	slug = strings.ToLower(slug)
	slug = nonAlphaNumericRegExp.ReplaceAllString(slug, "-")
	slug = multipleHyphensRegExp.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "", fmt.Errorf("unable to generate slug for %q", gitRef)
	}
	return slug, nil
}

// CreateNamespace creates a new namespace for a Quay instance.
func CreateNamespace(ctx context.Context, kubeClient kubernetes.Interface, tektonClient tektonclientset.Interface, routeClient routeclientset.Interface, controllerClient client.Client, gitRef string, overrides map[string]interface{}) (string, error) {
	slug, err := GitRefSlug(gitRef)
	if err != nil {
		return "", err
	}

	namespaceName := fmt.Sprintf("quay-%s-%s", slug, rand.String(5))
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
			Annotations: map[string]string{
				"run-quay/git-ref": gitRef,
			},
			Labels: map[string]string{
				"run-quay": "true",
			},
		},
	}
	_, err = kubeClient.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("error creating namespace: %w", err)
	}

	// Workaround OSDOCS-3415
	time.Sleep(1 * time.Second)

	err = CreatePipelineObjects(ctx, controllerClient, namespaceName)
	if err != nil {
		return namespaceName, fmt.Errorf("error creating pipeline objects: %w", err)
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
						StringVal: gitRef,
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
		return namespaceName, fmt.Errorf("error creating pipeline run: %w", err)
	}

	var routeHostname string
	err = wait.PollImmediateWithContext(ctx, 200*time.Millisecond, 10*time.Second, func(ctx context.Context) (bool, error) {
		hostname, err := GetRouteHostname(ctx, routeClient, namespaceName)
		if err == errNoRoute {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("error getting route hostname: %w", err)
		}
		routeHostname = hostname
		return true, nil
	})
	if err != nil {
		return namespaceName, fmt.Errorf("error waiting for route hostname: %w", err)
	}

	config, err := RenderConfig(namespaceName, routeHostname, overrides)
	if err != nil {
		return namespaceName, fmt.Errorf("error rendering config: %w", err)
	}

	_, err = kubeClient.CoreV1().Secrets(namespaceName).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "quay-app-config",
		},
		Data: map[string][]byte{
			"config.yaml": []byte(config),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return namespaceName, fmt.Errorf("error creating config secret: %w", err)
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
				gitRef, err := ParseSource(r.FormValue("source"))
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				overrides := map[string]interface{}{}
				if err := yaml.Unmarshal([]byte(r.FormValue("config")), &overrides); err != nil {
					http.Error(w, fmt.Sprintf("invalid overrides: %v", err), http.StatusBadRequest)
				}
				namespaceName, err := CreateNamespace(ctx, kubeClient, tektonClient, routeClient, controllerClient, gitRef, overrides)
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
