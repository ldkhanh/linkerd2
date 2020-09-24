package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/linkerd/linkerd2/cli/flag"
	"github.com/linkerd/linkerd2/pkg/charts"
	l5dcharts "github.com/linkerd/linkerd2/pkg/charts/linkerd2"
	"github.com/linkerd/linkerd2/pkg/healthcheck"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/tree"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/helm/pkg/chartutil"
	"sigs.k8s.io/yaml"
)

const (

	// addOnChartsPath is where the linkerd2 add-ons will be present
	addOnChartsPath = "add-ons"

	configStage       = "config"
	controlPlaneStage = "control-plane"

	helmDefaultChartName = "linkerd2"
	helmDefaultChartDir  = "linkerd2"

	errMsgCannotInitializeClient = `Unable to install the Linkerd control plane. Cannot connect to the Kubernetes cluster:

%s

You can use the --ignore-cluster flag if you just want to generate the installation config.`

	errMsgGlobalResourcesExist = `Unable to install the Linkerd control plane. It appears that there is an existing installation:

%s

If you are sure you'd like to have a fresh install, remove these resources with:

    linkerd install --ignore-cluster | kubectl delete -f -

Otherwise, you can use the --ignore-cluster flag to overwrite the existing global resources.
`

	errMsgLinkerdConfigResourceConflict = "Can't install the Linkerd control plane in the '%s' namespace. Reason: %s.\nIf this is expected, use the --ignore-cluster flag to continue the installation.\n"
	errMsgGlobalResourcesMissing        = "Can't install the Linkerd control plane in the '%s' namespace. The required Linkerd global resources are missing.\nIf this is expected, use the --skip-checks flag to continue the installation.\n"
)

var (
	templatesConfigStage = []string{
		"templates/namespace.yaml",
		"templates/identity-rbac.yaml",
		"templates/controller-rbac.yaml",
		"templates/destination-rbac.yaml",
		"templates/heartbeat-rbac.yaml",
		"templates/web-rbac.yaml",
		"templates/serviceprofile-crd.yaml",
		"templates/trafficsplit-crd.yaml",
		"templates/proxy-injector-rbac.yaml",
		"templates/sp-validator-rbac.yaml",
		"templates/tap-rbac.yaml",
		"templates/psp.yaml",
	}

	templatesControlPlaneStage = []string{
		"templates/_config.tpl",
		"templates/_helpers.tpl",
		"templates/identity.yaml",
		"templates/controller.yaml",
		"templates/destination.yaml",
		"templates/heartbeat.yaml",
		"templates/web.yaml",
		"templates/proxy-injector.yaml",
		"templates/sp-validator.yaml",
		"templates/tap.yaml",
		"templates/linkerd-config-addons.yaml",
	}

	ignoreCluster bool
)

/* Commands */

/* The install commands all follow the same flow:
 * 1. Load default values from the Linkerd2 chart
 * 2. Apply flags to modify the values
 * 3. Render the chart using those values
 *
 * The individual commands (install, install config, and install control-plane)
 * differ in which flags are available to each, what pre-check validations
 * are done, and which subset of the chart is rendered.
 */

func newCmdInstallConfig(values *l5dcharts.Values) *cobra.Command {
	flags, flagSet := makeAllStageFlags(values)

	cmd := &cobra.Command{
		Use:   "config [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes cluster-wide resources to install Linkerd",
		Long: `Output Kubernetes cluster-wide resources to install Linkerd.

This command provides Kubernetes configs necessary to install cluster-wide
resources for the Linkerd control plane. This command should be followed by
"linkerd install control-plane".`,
		Example: `  # Default install.
  linkerd install config | kubectl apply -f -

  # Install Linkerd into a non-default namespace.
  linkerd install config -l linkerdtest | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := flag.ApplySetFlags(values, flags)
			if err != nil {
				return err
			}
			if !ignoreCluster {
				// Ensure k8s is reachable and that Linkerd is not already installed.
				if err := errAfterRunningChecks(values.Global.CNIEnabled); err != nil {
					if healthcheck.IsCategoryError(err, healthcheck.KubernetesAPIChecks) {
						fmt.Fprintf(os.Stderr, errMsgCannotInitializeClient, err)
					} else {
						fmt.Fprintf(os.Stderr, errMsgGlobalResourcesExist, err)
					}
					os.Exit(1)
				}
			}

			return render(os.Stdout, values, configStage)
		},
	}

	cmd.Flags().AddFlagSet(flagSet)

	return cmd
}

func newCmdInstallControlPlane(values *l5dcharts.Values) *cobra.Command {
	var skipChecks bool

	allStageFlags, allStageFlagSet := makeAllStageFlags(values)
	installOnlyFlags, installOnlyFlagSet := makeInstallFlags(values)
	installUpgradeFlags, installUpgradeFlagSet, err := makeInstallUpgradeFlags(values)
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}
	proxyFlags, proxyFlagSet := makeProxyFlags(values)

	flags := flattenFlags(allStageFlags, installOnlyFlags, installUpgradeFlags, proxyFlags)

	cmd := &cobra.Command{
		Use:   "control-plane [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes control plane resources to install Linkerd",
		Long: `Output Kubernetes control plane resources to install Linkerd.

This command provides Kubernetes configs necessary to install the Linkerd
control plane. It should be run after "linkerd install config".`,
		Example: `  # Default install.
  linkerd install control-plane | kubectl apply -f -

  # Install Linkerd into a non-default namespace.
  linkerd install control-plane -l linkerdtest | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !skipChecks {
				// check if global resources exist to determine if the `install config`
				// stage succeeded
				if err := errAfterRunningChecks(values.Global.CNIEnabled); err == nil {
					if healthcheck.IsCategoryError(err, healthcheck.KubernetesAPIChecks) {
						fmt.Fprintf(os.Stderr, errMsgGlobalResourcesMissing, controlPlaneNamespace)
					}
					os.Exit(1)
				}
			}
			if !ignoreCluster {
				// Ensure there is not already an existing Linkerd installation.
				err = errIfLinkerdConfigExists()
				if err != nil {
					fmt.Fprintf(os.Stderr, errMsgLinkerdConfigResourceConflict, controlPlaneNamespace, err.Error())
					os.Exit(1)
				}
			}
			return install(values, flags, controlPlaneStage)
		},
	}

	cmd.Flags().AddFlagSet(allStageFlagSet)
	cmd.Flags().AddFlagSet(installOnlyFlagSet)
	cmd.Flags().AddFlagSet(installUpgradeFlagSet)
	cmd.Flags().AddFlagSet(proxyFlagSet)

	return cmd
}

func newCmdInstall() *cobra.Command {
	values, err := l5dcharts.NewValues(false)

	allStageFlags, allStageFlagSet := makeAllStageFlags(values)
	installOnlyFlags, installOnlyFlagSet := makeInstallFlags(values)
	installUpgradeFlags, installUpgradeFlagSet, err := makeInstallUpgradeFlags(values)
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}
	proxyFlags, proxyFlagSet := makeProxyFlags(values)

	flags := flattenFlags(allStageFlags, installOnlyFlags, installUpgradeFlags, proxyFlags)

	cmd := &cobra.Command{
		Use:   "install [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes configs to install Linkerd",
		Long: `Output Kubernetes configs to install Linkerd.

This command provides all Kubernetes configs necessary to install the Linkerd
control plane.`,
		Example: `  # Default install.
  linkerd install | kubectl apply -f -

  # Install Linkerd into a non-default namespace.
  linkerd install -l linkerdtest | kubectl apply -f -

  # Installation may also be broken up into two stages by user privilege, via
  # subcommands.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return install(values, flags, "")
		},
	}

	cmd.Flags().AddFlagSet(allStageFlagSet)
	cmd.Flags().AddFlagSet(installOnlyFlagSet)
	cmd.Flags().AddFlagSet(installUpgradeFlagSet)
	cmd.Flags().AddFlagSet(proxyFlagSet)
	cmd.PersistentFlags().BoolVar(&ignoreCluster, "ignore-cluster", false,
		"Ignore the current Kubernetes cluster when checking for existing cluster configuration (default false)")

	cmd.AddCommand(newCmdInstallConfig(values))
	cmd.AddCommand(newCmdInstallControlPlane(values))

	return cmd
}

func install(values *l5dcharts.Values, flags []flag.Flag, stage string) error {
	err := flag.ApplySetFlags(values, flags)
	if err != nil {
		return err
	}

	var k8sAPI *k8s.KubernetesAPI

	if !ignoreCluster {
		// Ensure there is not already an existing Linkerd installation.
		k8sAPI, err = k8s.NewAPI(kubeconfigPath, kubeContext, impersonate, impersonateGroup, 30*time.Second)
		if err != nil {
			return err
		}
		stored, err := loadStoredValues(k8sAPI)
		if err != nil {
			return err
		}
		if stored != nil {
			fmt.Fprintf(os.Stderr, errMsgLinkerdConfigResourceConflict, controlPlaneNamespace, "Secret/linkerd-config-overrides already exists")
			os.Exit(1)
		}
	}

	err = initializeIssuerCredentials(k8sAPI, values)
	if err != nil {
		return err
	}

	err = validateValues(k8sAPI, values)
	if err != nil {
		return err
	}

	return render(os.Stdout, values, stage)
}

func render(w io.Writer, values *l5dcharts.Values, stage string) error {
	// Render raw values and create chart config
	rawValues, err := yaml.Marshal(values)
	if err != nil {
		return err
	}

	files := []*chartutil.BufferedFile{
		{Name: chartutil.ChartfileName},
	}

	addOns, err := l5dcharts.ParseAddOnValues(values)
	if err != nil {
		return err
	}

	// Initialize add-on sub-charts
	addOnCharts := make(map[string]*charts.Chart)
	for _, addOn := range addOns {
		addOnCharts[addOn.Name()] = &charts.Chart{
			Name:      addOn.Name(),
			Dir:       addOnChartsPath + "/" + addOn.Name(),
			Namespace: controlPlaneNamespace,
			RawValues: append(addOn.Values(), rawValues...),
			Files: []*chartutil.BufferedFile{
				{
					Name: chartutil.ChartfileName,
				},
				{
					Name: chartutil.ValuesfileName,
				},
			},
		}
	}

	if stage == "" || stage == configStage {
		for _, template := range templatesConfigStage {
			files = append(files,
				&chartutil.BufferedFile{Name: template},
			)
		}

		// Fill add-on's sub-charts with config templates
		for _, addOn := range addOns {
			addOnCharts[addOn.Name()].Files = append(addOnCharts[addOn.Name()].Files, addOn.ConfigStageTemplates()...)
		}
	}

	if stage == "" || stage == controlPlaneStage {
		for _, template := range templatesControlPlaneStage {
			files = append(files,
				&chartutil.BufferedFile{Name: template},
			)
		}

		// Fill add-on's sub-charts with control-plane templates
		for _, addOn := range addOns {
			addOnCharts[addOn.Name()].Files = append(addOnCharts[addOn.Name()].Files, addOn.ControlPlaneStageTemplates()...)
		}

	}

	// TODO refactor to use l5dcharts.LoadChart()
	chart := &charts.Chart{
		Name:      helmDefaultChartName,
		Dir:       helmDefaultChartDir,
		Namespace: controlPlaneNamespace,
		RawValues: rawValues,
		Files:     files,
	}
	buf, err := chart.Render()
	if err != nil {
		return err
	}

	for _, addon := range addOns {
		b, err := addOnCharts[addon.Name()].Render()
		if err != nil {
			return err
		}

		if _, err := buf.WriteString(b.String()); err != nil {
			return err
		}
	}

	overrides, err := renderOverrides(values, values.Global.Namespace)
	if err != nil {
		return err
	}
	buf.WriteString(yamlSep)
	buf.WriteString(string(overrides))

	_, err = w.Write(buf.Bytes())
	return err
}

// renderOverrides outputs the Secret/linkerd-config-overrides resource which
// contains the subset of the values which have been changed from their defaults.
// This secret is used by the upgrade command the load configuration which was
// specified at install time.  Note that if identity issuer credentials were
// supplied to the install command or if they were generated by the install
// command, those credentials will be saved here so that they are preserved
// during upgrade.  Note also that this Secret/linkerd-config-overrides
// resource is not part of the Helm chart and will not be present when installing
// with Helm.
func renderOverrides(values *l5dcharts.Values, namespace string) ([]byte, error) {
	defaults, err := l5dcharts.NewValues(false)
	if err != nil {
		return nil, err
	}
	values.Configs = l5dcharts.ConfigJSONs{}

	overrides, err := tree.Diff(defaults, values)
	if err != nil {
		return nil, err
	}

	overridesBytes, err := yaml.Marshal(overrides)
	if err != nil {
		return nil, err
	}

	secret := corev1.Secret{
		TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "linkerd-config-overrides",
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"linkerd-config-overrides": overridesBytes,
		},
	}
	bytes, err := yaml.Marshal(secret)
	if err != nil {
		return nil, err
	}
	return bytes, nil
}

func errAfterRunningChecks(cniEnabled bool) error {
	checks := []healthcheck.CategoryID{
		healthcheck.KubernetesAPIChecks,
		healthcheck.LinkerdPreInstallGlobalResourcesChecks,
	}
	hc := healthcheck.NewHealthChecker(checks, &healthcheck.Options{
		ControlPlaneNamespace: controlPlaneNamespace,
		KubeConfig:            kubeconfigPath,
		Impersonate:           impersonate,
		ImpersonateGroup:      impersonateGroup,
		KubeContext:           kubeContext,
		APIAddr:               apiAddr,
		CNIEnabled:            cniEnabled,
	})

	var k8sAPIError error
	errMsgs := []string{}
	hc.RunChecks(func(result *healthcheck.CheckResult) {
		if result.Err != nil {
			if ce, ok := result.Err.(*healthcheck.CategoryError); ok {
				if ce.Category == healthcheck.KubernetesAPIChecks {
					k8sAPIError = ce
				} else if re, ok := ce.Err.(*healthcheck.ResourceError); ok {
					// resource error, print in kind.group/name format
					for _, res := range re.Resources {
						errMsgs = append(errMsgs, res.String())
					}
				} else {
					// unknown category error, just print it
					errMsgs = append(errMsgs, result.Err.Error())
				}
			} else {
				// unknown error, just print it
				errMsgs = append(errMsgs, result.Err.Error())
			}
		}
	})

	// errors from the KubernetesAPIChecks category take precedence
	if k8sAPIError != nil {
		return k8sAPIError
	}

	if len(errMsgs) > 0 {
		return errors.New(strings.Join(errMsgs, "\n"))
	}

	return nil
}

func errIfLinkerdConfigExists() error {
	kubeAPI, err := k8s.NewAPI(kubeconfigPath, kubeContext, impersonate, impersonateGroup, 0)
	if err != nil {
		return err
	}

	_, err = kubeAPI.CoreV1().Namespaces().Get(controlPlaneNamespace, metav1.GetOptions{})
	if err != nil {
		return err
	}

	_, _, err = healthcheck.FetchLinkerdConfigMap(kubeAPI, controlPlaneNamespace)
	if err == nil {
		return fmt.Errorf("'linkerd-config' configmap already exists")
	}
	if !kerrors.IsNotFound(err) {
		return err
	}

	values, err := loadStoredValues(kubeAPI)
	if err != nil {
		return err
	}
	if values != nil {
		return fmt.Errorf("'linkerd-config-overrides' secret already exists")
	}

	return nil
}
