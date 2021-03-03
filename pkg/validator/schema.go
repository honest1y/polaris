package validator

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gobuffalo/packr/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/fairwindsops/polaris/pkg/config"
	"github.com/fairwindsops/polaris/pkg/kube"
)

var (
	schemaBox     = (*packr.Box)(nil)
	builtInChecks = map[string]config.SchemaCheck{}
	// We explicitly set the order to avoid thrash in the
	// tests as we migrate toward JSON schema
	checkOrder = []string{
		// Controller Checks
		"multipleReplicasForDeployment",
		// Pod checks
		"hostIPCSet",
		"hostPIDSet",
		"hostNetworkSet",
		// Container checks
		"memoryLimitsMissing",
		"memoryRequestsMissing",
		"cpuLimitsMissing",
		"cpuRequestsMissing",
		"readinessProbeMissing",
		"livenessProbeMissing",
		"pullPolicyNotAlways",
		"tagNotSpecified",
		"hostPortSet",
		"runAsRootAllowed",
		"runAsPrivileged",
		"notReadOnlyRootFilesystem",
		"privilegeEscalationAllowed",
		"dangerousCapabilities",
		"insecureCapabilities",
		"priorityClassNotSet",
		// Other checks
		"tlsSettingsMissing",
		"pdbDisruptionsAllowedGreaterThanZero",
	}
)

func init() {
	schemaBox = packr.New("Schemas", "../../checks")
	for _, checkID := range checkOrder {
		contents, err := schemaBox.Find(checkID + ".yaml")
		if err != nil {
			panic(err)
		}
		check, err := parseCheck(contents)
		if err != nil {
			panic(err)
		}
		check.ID = checkID
		builtInChecks[checkID] = check
	}
}

func parseCheck(rawBytes []byte) (config.SchemaCheck, error) {
	reader := bytes.NewReader(rawBytes)
	check := config.SchemaCheck{}
	d := yaml.NewYAMLOrJSONDecoder(reader, 4096)
	for {
		if err := d.Decode(&check); err != nil {
			if err == io.EOF {
				return check, nil
			}
			return check, fmt.Errorf("Decoding schema check failed: %v", err)
		}
	}
}

func resolveCheck(conf *config.Configuration, checkID, kind string, target config.TargetKind, meta metaV1.Object, containerName string, isInitContainer bool) (*config.SchemaCheck, error) {
	check, ok := conf.CustomChecks[checkID]
	if !ok {
		check, ok = builtInChecks[checkID]
	}
	if !ok {
		return nil, fmt.Errorf("Check %s not found", checkID)
	}

	namespace := meta.GetNamespace()
	name := meta.GetName()
	if !conf.IsActionable(check.ID, namespace, name, containerName) {
		return nil, nil
	}
	if !check.IsActionable(target, kind, isInitContainer) {
		return nil, nil
	}
	return &check, nil
}

func makeResult(conf *config.Configuration, check *config.SchemaCheck, passes bool) ResultMessage {
	result := ResultMessage{
		ID:       check.ID,
		Severity: conf.Checks[check.ID],
		Category: check.Category,
		Success:  passes,
	}
	if passes {
		result.Message = check.SuccessMessage
	} else {
		result.Message = check.FailureMessage
	}
	return result
}

const exemptionAnnotationKey = "polaris.fairwinds.com/exempt"
const exemptionAnnotationPattern = "polaris.fairwinds.com/%s-exempt"

func hasExemptionAnnotation(ctrl kube.GenericWorkload, checkID string) bool {
	annot := ctrl.ObjectMeta.GetAnnotations()
	val := annot[exemptionAnnotationKey]
	if strings.ToLower(val) == "true" {
		return true
	}
	checkKey := fmt.Sprintf(exemptionAnnotationPattern, checkID)
	val = annot[checkKey]
	if strings.ToLower(val) == "true" {
		return true
	}
	return false
}

func applyPodSchemaChecks(conf *config.Configuration, controller kube.GenericWorkload) (ResultSet, error) {
	results := ResultSet{}
	checkIDs := getSortedKeys(conf.Checks)
	for _, checkID := range checkIDs {
		if !conf.DisallowExemptions && hasExemptionAnnotation(controller, checkID) {
			continue
		}
		check, err := resolveCheck(conf, checkID, controller.Kind, config.TargetPod, controller.ObjectMeta, "", false)

		if err != nil {
			return nil, err
		} else if check == nil {
			continue
		}
		passes, err := check.CheckPod(&controller.PodSpec)
		if err != nil {
			return nil, err
		}
		results[check.ID] = makeResult(conf, check, passes)
	}
	return results, nil
}

func applyControllerSchemaChecks(conf *config.Configuration, controller kube.GenericWorkload) (ResultSet, error) {
	results := ResultSet{}
	checkIDs := getSortedKeys(conf.Checks)
	for _, checkID := range checkIDs {
		if !conf.DisallowExemptions && hasExemptionAnnotation(controller, checkID) {
			continue
		}
		check, err := resolveCheck(conf, checkID, controller.Kind, config.TargetController, controller.ObjectMeta, "", false)

		if err != nil {
			return nil, err
		} else if check == nil {
			continue
		}
		passes, err := check.CheckController(controller.OriginalObjectJSON)
		if err != nil {
			return nil, err
		}
		results[check.ID] = makeResult(conf, check, passes)
	}
	return results, nil
}

func applyContainerSchemaChecks(conf *config.Configuration, controller kube.GenericWorkload, container *corev1.Container, isInit bool) (ResultSet, error) {
	results := ResultSet{}
	checkIDs := getSortedKeys(conf.Checks)
	for _, checkID := range checkIDs {
		if !conf.DisallowExemptions && hasExemptionAnnotation(controller, checkID) {
			continue
		}
		check, err := resolveCheck(conf, checkID, controller.Kind, config.TargetContainer, controller.ObjectMeta, container.Name, isInit)

		if err != nil {
			return nil, err
		} else if check == nil {
			continue
		}
		var passes bool
		if check.SchemaTarget == config.TargetPod {
			podCopy := controller.PodSpec
			podCopy.InitContainers = []corev1.Container{}
			podCopy.Containers = []corev1.Container{*container}
			passes, err = check.CheckPod(&podCopy)
		} else {
			passes, err = check.CheckContainer(container)
		}
		if err != nil {
			return nil, err
		}
		results[check.ID] = makeResult(conf, check, passes)
	}
	return results, nil
}

func applyOtherSchemaChecks(conf *config.Configuration, unst *unstructured.Unstructured) (ResultSet, error) {
	results := ResultSet{}
	checkIDs := getSortedKeys(conf.Checks)
	objMeta, err := meta.Accessor(unst)
	if err != nil {
		return results, err
	}
	for _, checkID := range checkIDs {
		check, err := resolveCheck(conf, checkID, unst.GetKind(), "", objMeta, "", false)

		if err != nil {
			return nil, err
		} else if check == nil {
			continue
		}
		passes, err := check.CheckObject(unst)
		if err != nil {
			return nil, err
		}
		results[check.ID] = makeResult(conf, check, passes)
	}
	return results, nil
}

func applyArbitrarySchemaChecks(conf *config.Configuration, unst *unstructured.Unstructured) (ResultSet, error) {
	results := ResultSet{}
	objMeta, err := meta.Accessor(unst)
	if err != nil {
		return results, err
	}
	checkIDs := getSortedKeys(conf.Checks)
	for _, checkID := range checkIDs {
		check, err := resolveCheck(conf, checkID, unst.GetKind(), config.TargetOther, objMeta, "", false)

		if err != nil {
			return nil, err
		} else if check == nil {
			continue
		}
		passes, err := check.CheckObject(unst)
		if err != nil {
			return nil, err
		}
		results[check.ID] = makeResult(conf, check, passes)
	}
	return results, nil
}

func getSortedKeys(m map[string]config.Severity) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
