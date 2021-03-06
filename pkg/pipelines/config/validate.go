package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mkmik/multierror"
	"github.com/redhat-developer/kam/pkg/pipelines/scm"
	"k8s.io/apimachinery/pkg/api/validation"
	"knative.dev/pkg/apis"
)

const (
	longServiceName  = "a service name cannot exceed 47 characters"
	serviceNameLimit = 47
)

type validateVisitor struct {
	errs         []error
	envNames     map[string]bool
	appNames     map[string]bool
	serviceNames map[string]bool
	serviceURLs  map[string][]string
	configNames  map[string]bool
}

// Validate validates the Manifest, returning a multi-error representing all the
// errors that were detected.
func (m *Manifest) Validate() error {
	vv := &validateVisitor{
		errs:         []error{},
		envNames:     map[string]bool{},
		appNames:     map[string]bool{},
		serviceNames: map[string]bool{},
		serviceURLs:  map[string][]string{},
		configNames:  map[string]bool{},
	}

	vv.errs = append(vv.errs, vv.validateConfig(m)...)
	err := m.Walk(vv)
	if err != nil {
		vv.errs = append(vv.errs, err)
	}
	vv.errs = append(vv.errs, vv.validateServiceURLs(m.GitOpsURL)...)

	if len(vv.errs) == 0 {
		return nil
	}
	return multierror.Join(vv.errs)
}

func (vv *validateVisitor) validateServiceURLs(gitOpsURL string) []error {
	errs := []error{}

	// all services must be the same git type as the gitops repo
	var gitType string

	if gitOpsURL != "" {
		gitOpsDriver, err := scm.GetDriverName(gitOpsURL)
		if err != nil {
			errs = append(errs, err)
		}
		gitType = gitOpsDriver
	}

	for url, paths := range vv.serviceURLs {
		if gitType != "" {
			serviceDriver, err := scm.GetDriverName(url)
			if err != nil {
				errs = append(errs, err)
			} else if gitType != serviceDriver {
				errs = append(errs, inconsistentGitTypeError(gitType, url, paths))
			}
		}
		if len(paths) > 1 {
			errs = append(errs, duplicateSourceError(url, paths))
		}
	}
	return errs
}

func (vv *validateVisitor) Environment(env *Environment) error {
	envPath := yamlPath(PathForEnvironment(env))
	if _, ok := vv.configNames[env.Name]; ok {
		vv.errs = append(vv.errs, invalidEnvironment(env.Name, "Environment name cannot be the same as a config name.", []string{envPath}))
	}
	if err := checkDuplicate(env.Name, envPath, vv.envNames); err != nil {
		vv.errs = append(vv.errs, err)
	}
	if err := validateName(env.Name, envPath); err != nil {
		vv.errs = append(vv.errs, err)
	}
	if err := validatePipelines(env.Pipelines, envPath); err != nil {
		vv.errs = append(vv.errs, err...)
	}
	return nil
}

func (vv *validateVisitor) Application(env *Environment, app *Application) error {
	appPath := yamlPath(PathForApplication(env, app))
	if err := checkDuplicate(app.Name, appPath, vv.appNames); err != nil {
		vv.errs = append(vv.errs, err)
	}
	if err := validateName(app.Name, appPath); err != nil {
		vv.errs = append(vv.errs, err)
	}

	if len(app.Services) == 0 && app.ConfigRepo == nil {
		vv.errs = append(vv.errs, missingFieldsError([]string{"services", "config_repo"}, []string{appPath}))
	}
	if len(app.Services) > 0 && app.ConfigRepo != nil {
		vv.errs = append(vv.errs, apis.ErrMultipleOneOf(yamlJoin(appPath, "services"), yamlJoin(appPath, "config_repo")))
	}

	if app.ConfigRepo != nil {
		vv.errs = append(vv.errs, validateConfigRepo(app.ConfigRepo, yamlJoin(appPath, "config_repo"))...)
	}
	if len(app.Services) > 0 {
		for _, r := range app.Services {
			_, ok := vv.serviceNames[r.Name]
			if !ok {
				vv.errs = append(vv.errs, missingServiceError(app.Name, []string{appPath}))
			}
		}
	}
	return nil
}

func (vv *validateVisitor) Service(app *Application, env *Environment, svc *Service) error {
	svcPath := yamlPath(PathForService(app, env, svc.Name))
	svcRelativePath := yamlPath(filepath.Join(env.Name, svc.Name))
	if svc.SourceURL != "" {
		previous, ok := vv.serviceURLs[svc.SourceURL]
		if !ok {
			previous = []string{}
		}
		previous = append(previous, svcPath)
		vv.serviceURLs[svc.SourceURL] = previous
	}
	if err := checkDuplicateService(svc.Name, svcPath, svcRelativePath, vv.serviceNames); err != nil {
		vv.errs = append(vv.errs, err)
	}
	if err := validateName(svc.Name, svcPath); err != nil {
		vv.errs = append(vv.errs, err)
	}

	if len(svc.Name) > serviceNameLimit {
		vv.errs = append(vv.errs, invalidNameError(svc.Name, longServiceName, []string{svcPath}))
	}
	if err := validateWebhook(svc.Webhook, svcPath); err != nil {
		vv.errs = append(vv.errs, err...)
	}
	if err := validatePipelines(svc.Pipelines, svcPath); err != nil {
		vv.errs = append(vv.errs, err...)
	}
	vv.serviceNames[svc.Name] = true
	return nil
}

func validateConfigRepo(repo *Repository, path string) []error {
	missingFields := []string{}
	errs := []error{}
	if repo.URL == "" {
		missingFields = append(missingFields, "url")
	}
	if repo.Path == "" {
		missingFields = append(missingFields, "path")
	}
	if len(missingFields) > 0 {
		errs = append(errs, missingFieldsError(missingFields, []string{path}))
	}
	return errs
}

func validateWebhook(hook *Webhook, path string) []error {
	errs := []error{}
	if hook == nil {
		return nil
	}
	if hook.Secret == nil {
		return list(missingFieldsError([]string{"secret"}, []string{yamlJoin(path, "webhook")}))
	}
	if err := validateName(hook.Secret.Name, yamlJoin(path, "webhook", "secret", "name")); err != nil {
		errs = append(errs, err)
	}
	if err := validateName(hook.Secret.Namespace, yamlJoin(path, "webhook", "secret", "namespace")); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func validatePipelines(pipelines *Pipelines, path string) []error {
	errs := []error{}
	if pipelines == nil {
		return nil
	}
	if pipelines.Integration == nil {
		return list(missingFieldsError([]string{"integration"}, []string{yamlJoin(path, "pipelines")}))
	}
	for _, name := range pipelines.Integration.Bindings {
		if err := validateName(name, yamlJoin(path, "pipelines", "integration", "binding")); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
func (vv *validateVisitor) validateConfig(manifest *Manifest) []error {
	errs := []error{}
	if manifest.Config != nil {
		if manifest.Config.ArgoCD != nil {
			if err := validateName(manifest.Config.ArgoCD.Namespace, yamlPath(PathForArgoCD())); err != nil {
				errs = append(errs, err)
			}
			vv.configNames[manifest.Config.ArgoCD.Namespace] = true
		}
		if manifest.Config.Pipelines != nil {
			if err := validateName(manifest.Config.Pipelines.Name, yamlPath(PathForPipelines(manifest.Config.Pipelines))); err != nil {
				errs = append(errs, err)
			}
			vv.configNames[manifest.Config.Pipelines.Name] = true
		}
	}
	return errs
}

func validateName(name, path string) *apis.FieldError {
	err := validation.NameIsDNS1035Label(name, true)
	if len(err) > 0 {
		return invalidNameError(name, err[0], []string{path})
	}
	return nil
}

func yamlPath(path string) string {
	return strings.ReplaceAll(path, "/", ".")
}

func yamlJoin(a string, b ...string) string {
	for _, s := range b {
		a = a + "." + s
	}
	return a
}

func list(errs ...error) []error {
	return errs
}

func invalidEnvironment(name, details string, paths []string) *apis.FieldError {
	return &apis.FieldError{
		Message: fmt.Sprintf("invalid environment %q", name),
		Details: details,
		Paths:   paths,
	}
}

func invalidNameError(name, details string, paths []string) *apis.FieldError {
	return &apis.FieldError{
		Message: fmt.Sprintf("invalid name %q", name),
		Details: details,
		Paths:   paths,
	}
}

func missingFieldsError(fields, paths []string) *apis.FieldError {
	return &apis.FieldError{
		Message: fmt.Sprintf("missing field(s) %v", strings.Join(addQuotes(fields...), ",")),
		Paths:   paths,
	}
}

func duplicateFieldsError(fields, paths []string) *apis.FieldError {
	return &apis.FieldError{
		Message: fmt.Sprintf("duplicate field(s) %v", strings.Join(addQuotes(fields...), ",")),
		Paths:   paths,
	}
}

func missingServiceError(app string, paths []string) *apis.FieldError {
	return &apis.FieldError{
		Message: fmt.Sprintf("missing service app %q", app),
		Paths:   paths,
	}
}

func duplicateSourceError(url string, paths []string) *apis.FieldError {
	return &apis.FieldError{
		Message: fmt.Sprintf("duplicate source detected, multiple services cannot share the same source repository: %s", url),
		Paths:   paths,
	}
}

func inconsistentGitTypeError(gitType, serviceURL string, paths []string) *apis.FieldError {
	return &apis.FieldError{
		Message: fmt.Sprintf("service URL must be a %s repository: %v", gitType, serviceURL),
		Paths:   paths,
	}
}

func addQuotes(items ...string) []string {
	quotes := []string{}
	for _, item := range items {
		quotes = append(quotes, fmt.Sprintf("%q", item))
	}
	return quotes
}

func checkDuplicate(field, path string, checkMap map[string]bool) error {
	_, ok := checkMap[path]
	if ok {
		return duplicateFieldsError([]string{field}, []string{path})
	}
	checkMap[path] = true
	return nil
}

func checkDuplicateService(field, path, relativePath string, checkMap map[string]bool) error {
	_, ok := checkMap[relativePath]
	if ok {
		return duplicateFieldsError([]string{field}, []string{path})
	}
	checkMap[relativePath] = true
	return nil
}
