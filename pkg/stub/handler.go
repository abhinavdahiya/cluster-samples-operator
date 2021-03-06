package stub

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/operator-framework/operator-sdk/pkg/util/k8sutil"

	"github.com/openshift/cluster-samples-operator/pkg/apis/samplesoperator/v1alpha1"

	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	kapis "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/operator-framework/operator-sdk/pkg/k8sclient"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	restclient "k8s.io/client-go/rest"

	imagev1 "github.com/openshift/api/image/v1"
	templatev1 "github.com/openshift/api/template/v1"

	imagev1client "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	templatev1client "github.com/openshift/client-go/template/clientset/versioned/typed/template/v1"
)

const (
	x86OCPContentRootDir   = "/opt/openshift/operator/ocp-x86_64"
	x86OKDContentRootDir   = "/opt/openshift/operator/okd-x86_64"
	ppc64OCPContentRootDir = "/opt/openshift/operator/ocp-ppc64le"
	x86                    = "x86_64"
	ppc                    = "ppc64le"
)

func NewHandler() sdk.Handler {
	h := Handler{}

	h.initter = &defaultInClusterInitter{h: &h}
	h.initter.init()

	h.sdkwrapper = &defaultSDKWrapper{h: &h}

	h.fileimagegetter = &defaultImageStreamFromFileGetter{h: &h}
	h.filetemplategetter = &defaultTemplateFromFileGetter{h: &h}
	h.filefinder = &defaultResourceFileLister{h: &h}

	h.imageclientwrapper = &defaultImageStreamClientWrapper{h: &h}
	h.templateclientwrapper = &defaultTemplateClientWrapper{h: &h}
	h.secretclientwrapper = &defaultSecretClientWrapper{h: &h}

	h.namespace = getNamespace()

	h.skippedImagestreams = make(map[string]bool)
	h.skippedTemplates = make(map[string]bool)

	h.mutex = &sync.Mutex{}
	timer := time.NewTimer(5 * time.Second)
	go func() {
		<-timer.C
		h.CreateDefaultResourceIfNeeded()
	}()

	return &h
}

type Handler struct {
	initter InClusterInitter

	sdkwrapper SDKWrapper

	samplesResource *v1alpha1.SamplesResource
	registrySecret  *corev1.Secret

	restconfig  *restclient.Config
	tempclient  *templatev1client.TemplateV1Client
	imageclient *imagev1client.ImageV1Client
	coreclient  *corev1client.CoreV1Client

	imageclientwrapper    ImageStreamClientWrapper
	templateclientwrapper TemplateClientWrapper
	secretclientwrapper   SecretClientWrapper

	fileimagegetter    ImageStreamFromFileGetter
	filetemplategetter TemplateFromFileGetter
	filefinder         ResourceFileLister

	namespace string

	skippedTemplates    map[string]bool
	skippedImagestreams map[string]bool

	mutex *sync.Mutex
}

func (h *Handler) GoodConditionUpdate(srcfg *v1alpha1.SamplesResource, newStatus corev1.ConditionStatus, condition v1alpha1.SamplesResourceConditionType) error {
	status := srcfg.Condition(condition)
	// decision was made to not spam master if
	// duplicate events come it (i.e. status does not
	// change)
	if status.Status != newStatus {
		now := kapis.Now()
		status.LastUpdateTime = now
		status.Status = newStatus
		status.LastTransitionTime = now
		srcfg.ConditionUpdate(status)
		err := h.sdkwrapper.Update(srcfg)
		if err != nil {
			if !kerrors.IsConflict(err) {
				return h.processError(srcfg, v1alpha1.SamplesUpdateFailed, err, "failed adding success condition to config: %v")
			}
			logrus.Printf("got conflict error %#v on success status update, going retry", err)
			srcfg, err = h.sdkwrapper.Get(srcfg.Name, srcfg.Namespace)
			if err != nil {
				return h.processError(srcfg, v1alpha1.SamplesUpdateFailed, err, "failed to retrieve samples resource after update conflict: %v")
			}
			status = srcfg.Condition(condition)
			status.LastTransitionTime = now
			status.LastUpdateTime = now
			status.Status = newStatus
			srcfg.ConditionUpdate(status)
			err = h.sdkwrapper.Update(srcfg)
			if err != nil {
				// just give up this time
				return h.processError(srcfg, v1alpha1.SamplesUpdateFailed, err, "failed to update status after conflict retry: %v")
			}
		}

		logrus.Println("")
		logrus.Println("")
		logrus.Println("")
		logrus.Println("")
	}
	h.samplesResource = srcfg
	return nil
}

func (h *Handler) CreateDefaultResourceIfNeeded() error {
	// coordinate with event handler processing
	// where it will set h.sampleResource
	// when it completely updates all imagestreams/templates/statuses
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if h.samplesResource == nil {
		// "4a" in the "startup" workflow, just create default
		// resource and set up that way
		srcfg := &v1alpha1.SamplesResource{}
		srcfg.Name = v1alpha1.SamplesResourceName
		srcfg.Namespace = h.namespace
		srcfg.Kind = "SamplesResource"
		srcfg.APIVersion = "samplesoperator.config.openshift.io/v1alpha1"
		srcfg.Spec.Architectures = append(srcfg.Spec.Architectures, x86)
		srcfg.Spec.InstallType = v1alpha1.CentosSamplesDistribution
		logrus.Println("creating default SamplesResource")
		err := h.sdkwrapper.Create(srcfg)
		if err != nil {
			if !kerrors.IsAlreadyExists(err) {
				return h.processError(srcfg, v1alpha1.SamplesUpdateFailed, err, "failed creating default resource: %v")
			}
			logrus.Println("got already exists error on create default, just going to forgo this time vs. retry")
			// just return the raw error and initiate the sdk's requeue/ratelimiter setup
			return err
		}
		return h.GoodConditionUpdate(srcfg, corev1.ConditionTrue, v1alpha1.SamplesExist)
	}
	logrus.Println("SampleResource already received, not creating default")
	return nil
}

func (h *Handler) manageDockerCfgSecret(deleted bool, samplesResource *v1alpha1.SamplesResource, secret *corev1.Secret) error {
	if secret.Name != v1alpha1.SamplesRegistryCredentials {
		return nil
	}
	if h.samplesResource == nil {
		if !deleted {
			h.registrySecret = secret
		}
		return nil
	}

	var newStatus corev1.ConditionStatus
	if deleted {
		err := h.secretclientwrapper.Delete("openshift", secret.Name, &metav1.DeleteOptions{})
		if err != nil && !kerrors.IsNotFound(err) {
			return h.processError(samplesResource, v1alpha1.SecretUpdateFailed, err, "failed to delete before create dockerconfig secret the openshift namespace: %v")
		}
		logrus.Printf("registry dockerconfig secret %s was deleted", secret.Name)
		newStatus = corev1.ConditionFalse
		h.registrySecret = nil
	} else {
		if h.registrySecret != nil {
			currentVersion, _ := strconv.Atoi(h.registrySecret.ResourceVersion)
			newVersion, _ := strconv.Atoi(secret.ResourceVersion)
			if newVersion <= currentVersion {
				return nil
			}
		}
		secretToCreate := corev1.Secret{}
		secret.DeepCopyInto(&secretToCreate)
		secretToCreate.Namespace = ""
		secretToCreate.ResourceVersion = ""
		secretToCreate.UID = ""

		s, err := h.secretclientwrapper.Get("openshift", secret.Name)
		if err != nil && !kerrors.IsNotFound(err) {
			return h.processError(samplesResource, v1alpha1.SecretUpdateFailed, err, "failed to get registry dockerconfig secret in openshift namespace : %v")
		}
		if err != nil {
			s = nil
		}
		if s != nil {
			logrus.Printf("updating dockerconfig secret %s in openshift namespace", v1alpha1.SamplesRegistryCredentials)
			_, err = h.secretclientwrapper.Update("openshift", &secretToCreate)
		} else {
			logrus.Printf("creating dockerconfig secret %s in openshift namespace", v1alpha1.SamplesRegistryCredentials)
			_, err = h.secretclientwrapper.Create("openshift", &secretToCreate)
		}
		if err != nil {
			return h.processError(samplesResource, v1alpha1.SecretUpdateFailed, err, "failed to create/update registry dockerconfig secret in openshif namespace : %v")
		}
		newStatus = corev1.ConditionTrue
		h.registrySecret = secret
	}

	err := h.GoodConditionUpdate(samplesResource, newStatus, v1alpha1.ImportCredentialsExist)
	if err != nil {
		return err
	}

	h.registrySecret = secret
	h.samplesResource = samplesResource

	return nil
}

func (h *Handler) Handle(ctx context.Context, event sdk.Event) error {
	switch event.Object.(type) {
	case *corev1.Secret:
		dockercfgSecret, _ := event.Object.(*corev1.Secret)

		err := h.manageDockerCfgSecret(event.Deleted, h.samplesResource, dockercfgSecret)
		if err != nil {
			return err
		}

	case *v1alpha1.SamplesResource:
		newStatus := corev1.ConditionTrue
		srcfg, _ := event.Object.(*v1alpha1.SamplesResource)
		if srcfg.Name != v1alpha1.SamplesResourceName {
			return nil
		}
		if event.Deleted {
			logrus.Println("SampleResource deleted")
			h.samplesResource = nil
			return h.CreateDefaultResourceIfNeeded()
		}

		// coordinate with timer's check on creating
		// default resource ... looks at h.sampleResource,
		// which is not set until this whole case is completed
		h.mutex.Lock()
		defer h.mutex.Unlock()

		if h.samplesResource != nil {
			currVersion, _ := strconv.Atoi(h.samplesResource.ResourceVersion)
			newVersion, _ := strconv.Atoi(srcfg.ResourceVersion)
			if newVersion <= currVersion {
				return nil
			}
		}

		// if the secret event came in before the samples resource event,
		// it could not be processed (though it would have been cached in
		// the Handler struct);  process it now
		if h.registrySecret != nil &&
			!srcfg.ConditionTrue(v1alpha1.ImportCredentialsExist) {
			h.manageDockerCfgSecret(false, srcfg, h.registrySecret)
		}

		h.buildSkipFilters(srcfg)

		if len(srcfg.Spec.Architectures) == 0 {
			srcfg.Spec.Architectures = append(srcfg.Spec.Architectures, x86)
		}

		if len(srcfg.Spec.InstallType) == 0 {
			srcfg.Spec.InstallType = v1alpha1.CentosSamplesDistribution
		}

		for _, arch := range srcfg.Spec.Architectures {
			dir, err := h.GetBaseDir(arch, srcfg)
			if err != nil {
				return h.processError(srcfg, v1alpha1.SamplesUpdateFailed, err, "error determining distro/type : %v")
			}
			files, err := h.filefinder.List(dir)
			if err != nil {
				return h.processError(srcfg, v1alpha1.SamplesUpdateFailed, err, "error reading in content : %v")
			}
			err = h.processFiles(dir, files, srcfg)
			if err != nil {
				return h.processError(srcfg, v1alpha1.SamplesUpdateFailed, err, "error processing content : %v")
			}

		}

		return h.GoodConditionUpdate(srcfg, newStatus, v1alpha1.SamplesExist)
	}
	return nil
}

func (h *Handler) buildSkipFilters(opcfg *v1alpha1.SamplesResource) {
	for _, st := range opcfg.Spec.SkippedTemplates {
		h.skippedTemplates[st] = true
	}
	for _, si := range opcfg.Spec.SkippedImagestreams {
		h.skippedImagestreams[si] = true
	}
}

func (h *Handler) processError(opcfg *v1alpha1.SamplesResource, ctype v1alpha1.SamplesResourceConditionType, err error, msg string, args ...interface{}) error {
	log := ""
	if args == nil {
		log = fmt.Sprintf(msg, err)
	} else {
		log = fmt.Sprintf(msg, err, args)
	}
	logrus.Println(log)
	var status *v1alpha1.SamplesResourceCondition
	switch ctype {
	case v1alpha1.SecretUpdateFailed:
		status = opcfg.Condition(v1alpha1.ImportCredentialsExist)
	case v1alpha1.SamplesUpdateFailed:
		status = opcfg.Condition(v1alpha1.SamplesExist)
	default:
		return err
	}
	// decision was made to not spam master if
	// duplicate events come it (i.e. status does not
	// change)
	if status.Status != corev1.ConditionUnknown {
		now := kapis.Now()
		status.LastUpdateTime = now
		status.Status = corev1.ConditionUnknown
		status.LastTransitionTime = now
		status.Message = log
		opcfg.ConditionUpdate(status)
		err2 := h.sdkwrapper.Update(opcfg)
		if err2 != nil {
			// just log this error
			logrus.Printf("failed to add error condition to config status: %v", err2)
		}
	}

	// return original error
	return err
}

func (h *Handler) processFiles(dir string, files []os.FileInfo, opcfg *v1alpha1.SamplesResource) error {
	for _, file := range files {
		if file.IsDir() {
			logrus.Printf("processing subdir %s from dir %s", file.Name(), dir)
			subfiles, err := h.filefinder.List(dir + "/" + file.Name())
			if err != nil {
				return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "error reading in content: %v")
			}
			err = h.processFiles(dir+"/"+file.Name(), subfiles, opcfg)
			if err != nil {
				return err
			}
		}
		logrus.Printf("processing file %s from dir %s", file.Name(), dir)

		if strings.HasSuffix(dir, "imagestreams") {
			imagestream, err := h.fileimagegetter.Get(dir + "/" + file.Name())
			if err != nil {
				return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "%v error reading file %s", dir+"/"+file.Name())
			}

			if _, isok := h.skippedImagestreams[imagestream.Name]; !isok {
				if opcfg.Spec.InstallType == v1alpha1.CentosSamplesDistribution {
					h.updateDockerPullSpec([]string{"docker.io"}, imagestream, opcfg)
				}
				if opcfg.Spec.InstallType == v1alpha1.RHELSamplesDistribution {
					h.updateDockerPullSpec([]string{"registry.redhat.io", "registry.access.redhat.com"}, imagestream, opcfg)
				}
				is, err := h.imageclientwrapper.Get("openshift", imagestream.Name, metav1.GetOptions{})
				if err != nil && !kerrors.IsNotFound(err) {
					return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "unexpected imagestream get error: %v")
				}

				if kerrors.IsNotFound(err) {
					// testing showed that we get an empty is vs. nil in this case
					is = nil
				}

				if is == nil {
					_, err = h.imageclientwrapper.Create("openshift", imagestream)
					if err != nil {
						return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "imagestream create error: %v")
					}
					logrus.Printf("created imagestream %s", imagestream.Name)
				} else {
					imagestream.ResourceVersion = is.ResourceVersion
					_, err = h.imageclientwrapper.Update("openshift", imagestream)
					if err != nil {
						return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "imagestream update error: %v")
					}
					logrus.Printf("updated imagestream %s", is.Name)
				}
			}
		}

		if strings.HasSuffix(dir, "templates") {
			template, err := h.filetemplategetter.Get(dir + "/" + file.Name())
			if err != nil {
				return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "%v error reading file %s", dir+"/"+file.Name())
			}

			if _, tok := h.skippedTemplates[template.Name]; !tok {
				t, err := h.templateclientwrapper.Get("openshift", template.Name, metav1.GetOptions{})
				if err != nil && !kerrors.IsNotFound(err) {
					return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "unexpected template get error: %v")
				}

				if kerrors.IsNotFound(err) {
					// testing showed that we get an empty is vs. nil in this case
					t = nil
				}

				if t == nil {
					_, err = h.templateclientwrapper.Create("openshift", template)
					if err != nil {
						return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "template create error: %v")
					}
					logrus.Printf("created template %s", template.Name)
				} else {
					template.ResourceVersion = t.ResourceVersion
					_, err = h.templateclientwrapper.Update("openshift", template)
					if err != nil {
						return h.processError(opcfg, v1alpha1.SamplesUpdateFailed, err, "template update error: %v")
					}
					logrus.Printf("updated template %s", t.Name)
				}
			}
		}
	}
	return nil
}

func (h *Handler) updateDockerPullSpec(oldies []string, imagestream *imagev1.ImageStream, opcfg *v1alpha1.SamplesResource) {
	if len(opcfg.Spec.SamplesRegistry) > 0 {
		replaced := false
		for _, old := range oldies {
			if strings.HasPrefix(imagestream.Spec.DockerImageRepository, old) {
				imagestream.Spec.DockerImageRepository = strings.Replace(imagestream.Spec.DockerImageRepository, old, opcfg.Spec.SamplesRegistry, 1)
				replaced = true
				break
			}
		}
		if !replaced {
			opcfg.Spec.SamplesRegistry = opcfg.Spec.SamplesRegistry + "/" + imagestream.Spec.DockerImageRepository
		}

		for _, tagref := range imagestream.Spec.Tags {
			replaced := false
			for _, old := range oldies {
				if tagref.From != nil && tagref.From.Kind == "DockerImage" {
					if strings.HasPrefix(tagref.From.Name, old) {
						tagref.From.Name = strings.Replace(tagref.From.Name, old, opcfg.Spec.SamplesRegistry, 1)
						replaced = true
						break
					}
				}
			}
			if !replaced {
				tagref.From.Name = opcfg.Spec.SamplesRegistry + "/" + tagref.From.Name
			}
		}
	}

}

func (h *Handler) GetBaseDir(arch string, opcfg *v1alpha1.SamplesResource) (dir string, err error) {
	switch arch {
	case x86:
		switch opcfg.Spec.InstallType {
		case v1alpha1.RHELSamplesDistribution:
			dir = x86OCPContentRootDir
		case v1alpha1.CentosSamplesDistribution:
			dir = x86OKDContentRootDir
		default:
			err = fmt.Errorf("invalid install type %s specified, should be rhel or centos", string(opcfg.Spec.InstallType))
		}
	case ppc:
		switch opcfg.Spec.InstallType {
		case v1alpha1.CentosSamplesDistribution:
			err = fmt.Errorf("ppc64le architecture and centos install are not currently supported")
		case v1alpha1.RHELSamplesDistribution:
			dir = ppc64OCPContentRootDir
		default:
			err = fmt.Errorf("invalid install type %s specified, should be rhel or centos", string(opcfg.Spec.InstallType))
		}
	default:
		err = fmt.Errorf("architecture %s unsupported; only support %s and %s", arch, x86, ppc)
	}
	return dir, err
}

func getTemplateClient(restconfig *restclient.Config) (*templatev1client.TemplateV1Client, error) {
	return templatev1client.NewForConfig(restconfig)
}

func getImageClient(restconfig *restclient.Config) (*imagev1client.ImageV1Client, error) {
	return imagev1client.NewForConfig(restconfig)
}

func getRestConfig() (*restclient.Config, error) {
	// Build a rest.Config from configuration injected into the Pod by
	// Kubernetes.  Clients will use the Pod's ServiceAccount principal.
	return restclient.InClusterConfig()
}

func getNamespace() string {
	b, _ := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/" + corev1.ServiceAccountNamespaceKey)
	return string(b)
}

type ImageStreamClientWrapper interface {
	Get(namespace, name string, opts metav1.GetOptions) (*imagev1.ImageStream, error)
	List(namespace string, opts metav1.ListOptions) (*imagev1.ImageStreamList, error)
	Create(namespace string, is *imagev1.ImageStream) (*imagev1.ImageStream, error)
	Update(namespace string, is *imagev1.ImageStream) (*imagev1.ImageStream, error)
}

type defaultImageStreamClientWrapper struct {
	h *Handler
}

func (g *defaultImageStreamClientWrapper) Get(namespace, name string, opts metav1.GetOptions) (*imagev1.ImageStream, error) {
	return g.h.imageclient.ImageStreams(namespace).Get(name, opts)
}

func (g *defaultImageStreamClientWrapper) List(namespace string, opts metav1.ListOptions) (*imagev1.ImageStreamList, error) {
	return g.h.imageclient.ImageStreams(namespace).List(opts)
}

func (g *defaultImageStreamClientWrapper) Create(namespace string, is *imagev1.ImageStream) (*imagev1.ImageStream, error) {
	return g.h.imageclient.ImageStreams(namespace).Create(is)
}

func (g *defaultImageStreamClientWrapper) Update(namespace string, is *imagev1.ImageStream) (*imagev1.ImageStream, error) {
	return g.h.imageclient.ImageStreams(namespace).Update(is)
}

type TemplateClientWrapper interface {
	Get(namespace, name string, opts metav1.GetOptions) (*templatev1.Template, error)
	List(namespace string, opts metav1.ListOptions) (*templatev1.TemplateList, error)
	Create(namespace string, t *templatev1.Template) (*templatev1.Template, error)
	Update(namespace string, t *templatev1.Template) (*templatev1.Template, error)
}

type defaultTemplateClientWrapper struct {
	h *Handler
}

func (g *defaultTemplateClientWrapper) Get(namespace, name string, opts metav1.GetOptions) (*templatev1.Template, error) {
	return g.h.tempclient.Templates(namespace).Get(name, opts)
}

func (g *defaultTemplateClientWrapper) List(namespace string, opts metav1.ListOptions) (*templatev1.TemplateList, error) {
	return g.h.tempclient.Templates(namespace).List(opts)
}

func (g *defaultTemplateClientWrapper) Create(namespace string, t *templatev1.Template) (*templatev1.Template, error) {
	return g.h.tempclient.Templates(namespace).Create(t)
}

func (g *defaultTemplateClientWrapper) Update(namespace string, t *templatev1.Template) (*templatev1.Template, error) {
	return g.h.tempclient.Templates(namespace).Update(t)
}

type SecretClientWrapper interface {
	Get(namespace, name string) (*corev1.Secret, error)
	Create(namespace string, s *corev1.Secret) (*corev1.Secret, error)
	Update(namespace string, s *corev1.Secret) (*corev1.Secret, error)
	Delete(namespace, name string, opts *metav1.DeleteOptions) error
}

type defaultSecretClientWrapper struct {
	h *Handler
}

func (g *defaultSecretClientWrapper) Get(namespace, name string) (*corev1.Secret, error) {
	return g.h.coreclient.Secrets(namespace).Get(name, metav1.GetOptions{})
}

func (g *defaultSecretClientWrapper) Create(namespace string, s *corev1.Secret) (*corev1.Secret, error) {
	return g.h.coreclient.Secrets(namespace).Create(s)
}

func (g *defaultSecretClientWrapper) Update(namespace string, s *corev1.Secret) (*corev1.Secret, error) {
	return g.h.coreclient.Secrets(namespace).Update(s)
}

func (g *defaultSecretClientWrapper) Delete(namespace, name string, opts *metav1.DeleteOptions) error {
	return g.h.coreclient.Secrets(namespace).Delete(name, opts)
}

type ImageStreamFromFileGetter interface {
	Get(fullFilePath string) (is *imagev1.ImageStream, err error)
}

type defaultImageStreamFromFileGetter struct {
	h *Handler
}

func (g *defaultImageStreamFromFileGetter) Get(fullFilePath string) (is *imagev1.ImageStream, err error) {
	isjsonfile, err := ioutil.ReadFile(fullFilePath)
	if err != nil {
		return nil, err
	}

	imagestream := &imagev1.ImageStream{}
	err = json.Unmarshal(isjsonfile, imagestream)
	if err != nil {
		return nil, err
	}

	return imagestream, nil
}

type TemplateFromFileGetter interface {
	Get(fullFilePath string) (t *templatev1.Template, err error)
}

type defaultTemplateFromFileGetter struct {
	h *Handler
}

func (g *defaultTemplateFromFileGetter) Get(fullFilePath string) (t *templatev1.Template, err error) {
	tjsonfile, err := ioutil.ReadFile(fullFilePath)
	if err != nil {
		return nil, err
	}
	template := &templatev1.Template{}
	err = json.Unmarshal(tjsonfile, template)
	if err != nil {
		return nil, err
	}

	return template, nil
}

type ResourceFileLister interface {
	List(dir string) (files []os.FileInfo, err error)
}

type defaultResourceFileLister struct {
	h *Handler
}

func (g *defaultResourceFileLister) List(dir string) (files []os.FileInfo, err error) {
	files, err = ioutil.ReadDir(dir)
	return files, err

}

type InClusterInitter interface {
	init()
}

type defaultInClusterInitter struct {
	h *Handler
}

func (g *defaultInClusterInitter) init() {
	restconfig, err := getRestConfig()
	if err != nil {
		logrus.Errorf("failed to get rest config : %v", err)
		panic(err)
	}
	g.h.restconfig = restconfig
	tempclient, err := getTemplateClient(restconfig)
	if err != nil {
		logrus.Errorf("failed to get template client : %v", err)
		panic(err)
	}
	g.h.tempclient = tempclient
	logrus.Printf("template client %#v", tempclient)
	imageclient, err := getImageClient(restconfig)
	if err != nil {
		logrus.Errorf("failed to get image client : %v", err)
		panic(err)
	}
	g.h.imageclient = imageclient
	logrus.Printf("image client %#v", imageclient)
	coreclient, err := corev1client.NewForConfig(restconfig)
	if err != nil {
		logrus.Errorf("failed to get core client : %v", err)
		panic(err)
	}
	g.h.coreclient = coreclient
}

type SDKWrapper interface {
	Update(samplesResource *v1alpha1.SamplesResource) (err error)
	Create(samplesResource *v1alpha1.SamplesResource) (err error)
	Get(name, namespace string) (*v1alpha1.SamplesResource, error)
}

type defaultSDKWrapper struct {
	h *Handler
}

func (g *defaultSDKWrapper) Update(samplesResource *v1alpha1.SamplesResource) error {
	return sdk.Update(samplesResource)
}

func (g *defaultSDKWrapper) Create(samplesResource *v1alpha1.SamplesResource) error {
	return sdk.Create(samplesResource)
}

func (g *defaultSDKWrapper) Get(name, namespace string) (*v1alpha1.SamplesResource, error) {
	resourceClient, _, err := k8sclient.GetResourceClient("samplesoperator.config.openshift.io/v1alpha1", "SamplesResource", namespace)
	if err != nil {
		return nil, err
	}
	uobj, err := resourceClient.Get(name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	obj := k8sutil.RuntimeObjectFromUnstructured(uobj)
	sr, ok := obj.(*v1alpha1.SamplesResource)
	if !ok {
		return nil, fmt.Errorf("runtime obj #%v not SamplesResource", obj)
	}
	return sr, nil
}
