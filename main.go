// Copyright (c) 2020 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/go-logr/zapr"
	"github.com/spf13/pflag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"open-cluster-management.io/addon-framework/pkg/lease"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	policyv1 "github.com/stolostron/config-policy-controller/api/v1"
	"github.com/stolostron/config-policy-controller/controllers"
	"github.com/stolostron/config-policy-controller/pkg/common"
	"github.com/stolostron/config-policy-controller/version"
)

// Change below variables to serve metrics on different host or port.
var (
	metricsHost       = "0.0.0.0"
	metricsPort int32 = 8383
	scheme            = k8sruntime.NewScheme()
	log               = ctrl.Log.WithName("setup")
)

func printVersion() {
	log.Info("Using", "OperatorVersion", version.Version, "GoVersion", runtime.Version(),
		"GOOS", runtime.GOOS, "GOARCH", runtime.GOARCH)
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
	utilruntime.Must(policyv1.AddToScheme(scheme))
}

type zapLevelFlag struct {
	zapcore.Level
}

var _ pflag.Value = &zapLevelFlag{}

// Set ensures that the level passed by a flag is valid
func (f *zapLevelFlag) Set(val string) error {
	level := strings.ToLower(val)
	switch level {
	case "debug":
		f.Level = zap.DebugLevel
	case "info":
		f.Level = zap.InfoLevel
	case "warn":
		f.Level = zap.WarnLevel
	case "error":
		f.Level = zap.ErrorLevel
	default:
		l, err := strconv.Atoi(level)
		if err != nil || l < 0 {
			return fmt.Errorf("invalid log level \"%s\"", val)
		}

		f.Level = zapcore.Level(int8(-1 * l))
	}

	return nil
}

func (f *zapLevelFlag) Type() string {
	return "level"
}

func main() {
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	var clusterName, hubConfigSecretNs, hubConfigSecretName, probeAddr, zapEncoder string
	var frequency, decryptionConcurrency uint
	var enableLease, enableLeaderElection, legacyLeaderElection bool
	var zapLevel zapLevelFlag

	pflag.UintVar(&frequency, "update-frequency", 10,
		"The status update frequency (in seconds) of a mutation policy")
	pflag.BoolVar(&enableLease, "enable-lease", false,
		"If enabled, the controller will start the lease controller to report its status")
	pflag.StringVar(&clusterName, "cluster-name", "acm-managed-cluster", "Name of the cluster")
	pflag.StringVar(&hubConfigSecretNs, "hubconfig-secret-ns", "open-cluster-management-agent-addon",
		"Namespace for hub config kube-secret")
	pflag.StringVar(&hubConfigSecretName, "hubconfig-secret-name", "policy-controller-hub-kubeconfig",
		"Name of the hub config kube-secret")
	pflag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	pflag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	pflag.BoolVar(&legacyLeaderElection, "legacy-leader-elect", false,
		"Use a legacy leader election method for controller manager instead of the lease API.")
	pflag.UintVar(
		&decryptionConcurrency,
		"decryption-concurrency",
		5,
		"The max number of concurrent policy template decryptions",
	)
	pflag.Var(&zapLevel, "zap-log-level", "Zap Level to configure the verbosity of logging.")
	pflag.StringVar(&zapEncoder, "zap-encoder", "console", "Zap log encoding (one of 'json' or 'console')")

	pflag.Parse()

	zapEncoderCfg := zap.NewProductionEncoderConfig()
	zapEncoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	zapEncoderCfg.EncodeLevel = func(l zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
		num := int8(l)
		if num > -2 { // These levels are known by zap, as "info", "error", etc.
			enc.AppendString(l.String())
		} else { // Zap doesn't like these levels as much. Format them like "lvl-n"
			enc.AppendString("lvl" + strconv.Itoa(int(num)))
		}
	}

	zapcfg := zap.Config{
		Level:         zap.NewAtomicLevelAt(zapLevel.Level),
		Encoding:      zapEncoder,
		EncoderConfig: zapEncoderCfg,
		OutputPaths:   []string{"stdout"},
	}

	zapLog, err := zapcfg.Build()
	if err != nil {
		panic(fmt.Sprintf("Failed to build zap Logger from configuration: %v", err))
	}

	ctrl.SetLogger(zapr.NewLogger(zapLog))

	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)

	// Sync the glog and klog flags (without this, setting -v=x will not adjust the klog level used by dependencies)
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			if err := f2.Value.Set(value); err != nil {
				if f1.Name != "log_backtrace_at" { // skip this one flag - klog doesn't like glog's default
					log.Error(err, "Unable to sync klog flag", "name", f1.Name, "desiredValue", f1.Value.String())
				}
			}
		}
	})

	// send klog messages through a zap logger so they have the same format
	if zapEncoder == "console" { // klog already adds a newline, so have zap skip adding one.
		zapcfg.EncoderConfig.SkipLineEnding = true
	}

	klogLevel, err := strconv.Atoi(pflag.Lookup("v").Value.String())
	if err != nil {
		log.Error(err, "Invalid value passed to 'v' flag - using '0' as a default")

		klogLevel = 0
	}

	zapcfg.Level = zap.NewAtomicLevelAt(zapcore.Level(int8(-1 * klogLevel)))

	kZapLog, err := zapcfg.Build()
	if err != nil {
		panic(fmt.Sprintf("Failed to build zap Logger for klog from configuration: %v", err))
	}

	klog.SetLogger(zapr.NewLogger(kZapLog).WithName("klog"))

	klog.V(0).Info("klog 0")
	klog.V(1).Info("klog 1")
	klog.V(2).Info("klog 2")
	klog.V(3).Info("klog 3")
	klog.V(4).Info("klog 4")
	klog.V(5).Info("klog 5")

	log.V(0).Info("zap 0")
	log.V(1).Info("zap 1")
	log.V(2).Info("zap 2")
	log.V(3).Info("zap 3")
	log.V(4).Info("zap 4")
	log.V(5).Info("zap 5")

	printVersion()

	namespace, err := common.GetWatchNamespace()
	if err != nil {
		log.Error(err, "Failed to get watch namespace")
		os.Exit(1)
	}

	log.V(2).Info("Configured the watch namespace", "namespace", namespace)

	// Get a config to talk to the apiserver
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "Failed to get config")
		os.Exit(1)
	}

	// Set default manager options
	options := manager.Options{
		Namespace:              namespace,
		MetricsBindAddress:     fmt.Sprintf("%s:%d", metricsHost, metricsPort),
		Scheme:                 scheme,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "config-policy-controller.open-cluster-management.io",
		// Disable the cache for Secrets to avoid a watch getting created when the `policy-encryption-key`
		// Secret is retrieved. Special cache handling is done by the controller.
		ClientDisableCacheFor: []client.Object{&corev1.Secret{}},
		// Override the EventBroadcaster so that the spam filter will not ignore events for the policy but with
		// different messages if a large amount of events for that policy are sent in a short time.
		EventBroadcaster: record.NewBroadcasterWithCorrelatorOptions(
			record.CorrelatorOptions{
				// This essentially disables event aggregation of the same events but with different messages.
				MaxIntervalInSeconds: 1,
				// This is the default spam key function except it adds the reason and message as well.
				// https://github.com/kubernetes/client-go/blob/v0.23.3/tools/record/events_cache.go#L70-L82
				SpamKeyFunc: func(event *corev1.Event) string {
					return strings.Join(
						[]string{
							event.Source.Component,
							event.Source.Host,
							event.InvolvedObject.Kind,
							event.InvolvedObject.Namespace,
							event.InvolvedObject.Name,
							string(event.InvolvedObject.UID),
							event.InvolvedObject.APIVersion,
							event.Reason,
							event.Message,
						},
						"",
					)
				},
			},
		),
	}

	if strings.Contains(namespace, ",") {
		options.Namespace = ""
		options.NewCache = cache.MultiNamespacedCacheBuilder(strings.Split(namespace, ","))
	}

	if legacyLeaderElection {
		// If legacyLeaderElection is enabled, then that means the lease API is not available.
		// In this case, use the legacy leader election method of a ConfigMap.
		log.Info("Using the legacy leader election of configmaps")

		options.LeaderElectionResourceLock = "configmaps"
	}

	// Create a new manager to provide shared dependencies and start components
	mgr, err := manager.New(cfg, options)
	if err != nil {
		log.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	reconciler := controllers.ConfigurationPolicyReconciler{
		Client:                mgr.GetClient(),
		DecryptionConcurrency: uint8(decryptionConcurrency),
		Scheme:                mgr.GetScheme(),
		Recorder:              mgr.GetEventRecorderFor(controllers.ControllerName),
	}
	if err = reconciler.SetupWithManager(mgr); err != nil {
		log.Error(err, "Unable to create controller", "controller", "ConfigurationPolicy")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "Unable to set up health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	// Initialize some variables
	clientset := kubernetes.NewForConfigOrDie(cfg)
	common.Initialize(clientset, cfg)
	controllers.Initialize(cfg, clientset, namespace)

	// PeriodicallyExecConfigPolicies is the go-routine that periodically checks the policies
	log.V(1).Info("Perodically processing Configuration Policies", "frequency", frequency)

	go reconciler.PeriodicallyExecConfigPolicies(frequency, mgr.Elected(), false)

	// This lease is not related to leader election. This is to report the status of the controller
	// to the addon framework. This can be seen in the "status" section of the ManagedClusterAddOn
	// resource objects.
	if enableLease {
		operatorNs, err := common.GetOperatorNamespace()
		if err != nil {
			if errors.Is(err, common.ErrNoNamespace) || errors.Is(err, common.ErrRunLocal) {
				log.Info("Skipping lease; not running in a cluster")
			} else {
				log.Error(err, "Failed to get operator namespace")
				os.Exit(1)
			}
		} else {
			log.V(2).Info("Got operator namespace", "Namespace", operatorNs)
			log.Info("Starting lease controller to report status")

			leaseUpdater := lease.NewLeaseUpdater(
				clientset,
				"config-policy-controller",
				operatorNs,
			)

			// set hubCfg on lease updated if found
			hubCfg, err := common.LoadHubConfig(hubConfigSecretNs, hubConfigSecretName)
			if err != nil {
				log.Error(err, "Could not load hub config, lease updater not set with config")
			} else {
				leaseUpdater = leaseUpdater.WithHubLeaseConfig(hubCfg, clusterName)
			}

			go leaseUpdater.Start(context.TODO())
		}
	} else {
		log.Info("Addon status reporting is not enabled")
	}

	log.Info("Starting manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
