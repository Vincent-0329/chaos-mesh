// Copyright 2019 Chaos Mesh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	grpcUtils "github.com/chaos-mesh/chaos-mesh/pkg/grpc"
	"github.com/chaos-mesh/chaos-mesh/pkg/mock"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/chaos-mesh/chaos-mesh/api/v1alpha1"
	ctrlconfig "github.com/chaos-mesh/chaos-mesh/controllers/config"
	daemonClient "github.com/chaos-mesh/chaos-mesh/pkg/chaosdaemon/client"
	"github.com/chaos-mesh/chaos-mesh/pkg/chaosdaemon/pb"
	"github.com/chaos-mesh/chaos-mesh/pkg/portforward"
	"github.com/chaos-mesh/chaos-mesh/pkg/selector"
)

type Color string

const (
	Blue    Color = "Blue"
	Red     Color = "Red"
	Green   Color = "Green"
	Cyan    Color = "Cyan"
	NoColor Color = ""
)

var (
	colorFunc = map[Color]func(string, ...interface{}){
		Blue:  color.Blue,
		Red:   color.Red,
		Green: color.Green,
		Cyan:  color.Cyan,
	}
	scheme = runtime.NewScheme()
)

// ClientSet contains two different clients
type ClientSet struct {
	CtrlCli client.Client
	KubeCli *kubernetes.Clientset
}

type ChaosResult struct {
	Name string
	Pods []PodResult
}

type PodResult struct {
	Name  string
	Items []ItemResult
}

const (
	ItemSuccess = iota + 1
	ItemFailure
)

type ItemResult struct {
	Name    string
	Value   string
	Status  int    `json:",omitempty"`
	SucInfo string `json:",omitempty"`
	ErrInfo string `json:",omitempty"`
}

func init() {
	_ = v1alpha1.AddToScheme(scheme)
	_ = clientgoscheme.AddToScheme(scheme)
}

func upperCaseChaos(str string) string {
	parts := regexp.MustCompile("(.*)(chaos)").FindStringSubmatch(str)
	return strings.Title(parts[1]) + strings.Title(parts[2])
}

// PrettyPrint print with tab number and color
func PrettyPrint(s string, indentLevel int, color Color) {
	var tabStr string
	for i := 0; i < indentLevel; i++ {
		tabStr += "\t"
	}
	str := fmt.Sprintf("%s%s\n\n", tabStr, regexp.MustCompile("\n").ReplaceAllString(s, "\n"+tabStr))
	if color != NoColor {
		if cfunc, ok := colorFunc[color]; !ok {
			fmt.Print("COLOR NOT SUPPORTED")
		} else {
			cfunc(str)
		}
	} else {
		fmt.Print(str)
	}
}

// PrintResult prints result to users in prettier format
func PrintResult(result []ChaosResult) {
	for _, chaos := range result {
		PrettyPrint("[Chaos]: "+chaos.Name, 0, Blue)
		for _, pod := range chaos.Pods {
			PrettyPrint("[Pod]: "+pod.Name, 0, Blue)
			for i, item := range pod.Items {
				PrettyPrint(fmt.Sprintf("%d. [%s]", i+1, item.Name), 1, Cyan)
				PrettyPrint(item.Value, 1, NoColor)
				if item.Status == ItemSuccess {
					if item.SucInfo != "" {
						PrettyPrint(item.SucInfo, 1, Green)
					} else {
						PrettyPrint("Execute as expected", 1, Green)
					}
				} else if item.Status == ItemFailure {
					PrettyPrint(fmt.Sprintf("Failed: %s ", item.ErrInfo), 1, Red)
				}
			}
		}
	}
}

// MarshalChaos returns json in readable format
func MarshalChaos(s interface{}) (string, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", errors.Wrapf(err, "failed to marshal indent")
	}
	return string(b), nil
}

// InitClientSet inits two different clients that would be used
func InitClientSet() (*ClientSet, error) {
	restconfig, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	ctrlClient, err := client.New(restconfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client")
	}
	kubeClient, err := kubernetes.NewForConfig(restconfig)
	if err != nil {
		return nil, errors.Wrap(err, "error in getting acess to k8s")
	}
	return &ClientSet{ctrlClient, kubeClient}, nil
}

// GetPods returns pod list and corresponding chaos daemon
func GetPods(ctx context.Context, chaosName string, status v1alpha1.ChaosStatus, selectorSpec v1alpha1.SelectorSpec, c client.Client) ([]v1.Pod, []v1.Pod, error) {
	// get podName
	failedMessage := status.FailedMessage
	if failedMessage != "" {
		PrettyPrint(fmt.Sprintf("chaos %s failed with: %s", chaosName, failedMessage), 0, Red)
	}

	phase := status.Experiment.Phase
	nextStart := status.Scheduler.NextStart

	if phase == v1alpha1.ExperimentPhaseWaiting {
		waitTime := nextStart.Sub(time.Now())
		L().WithName("GetPods").V(1).Info(fmt.Sprintf("Waiting for chaos %s to start, in %s\n", chaosName, waitTime))
		time.Sleep(waitTime)
	}

	pods, err := selector.SelectPods(ctx, c, c, selectorSpec, ctrlconfig.ControllerCfg.ClusterScoped, ctrlconfig.ControllerCfg.TargetNamespace, ctrlconfig.ControllerCfg.EnableFilterNamespace)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to SelectPods")
	}
	L().WithName("GetPods").V(4).Info("select pods for chaos", "chaos", chaosName, "pods", pods)
	if len(pods) == 0 {
		return nil, nil, fmt.Errorf("no pods found for chaos %s, selector: %s", chaosName, selectorSpec)
	}

	// TODO: replace select daemon by
	var chaosDaemons []v1.Pod
	// get chaos daemon
	for _, pod := range pods {
		nodeName := pod.Spec.NodeName
		daemonSelector := v1alpha1.SelectorSpec{
			Nodes:          []string{nodeName},
			LabelSelectors: map[string]string{"app.kubernetes.io/component": "chaos-daemon"},
		}
		daemons, err := selector.SelectPods(ctx, c, nil, daemonSelector, ctrlconfig.ControllerCfg.ClusterScoped, ctrlconfig.ControllerCfg.TargetNamespace, ctrlconfig.ControllerCfg.EnableFilterNamespace)
		if err != nil {
			return nil, nil, errors.Wrap(err, fmt.Sprintf("failed to select daemon pod for pod %s", pod.GetName()))
		}
		if len(daemons) == 0 {
			return nil, nil, fmt.Errorf("no daemons found for pod %s with selector: %s", pod.GetName(), daemonSelector)
		}
		chaosDaemons = append(chaosDaemons, daemons[0])
	}

	return pods, chaosDaemons, nil
}

// GetChaosList returns chaos list limited by input
func GetChaosList(ctx context.Context, chaosType string, chaosName string, ns string, c client.Client) ([]runtime.Object, []string, error) {
	chaosType = upperCaseChaos(strings.ToLower(chaosType))
	allKinds := v1alpha1.AllKinds()
	chaosListInterface := allKinds[chaosType].ChaosList

	if err := c.List(ctx, chaosListInterface, client.InNamespace(ns)); err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get chaosList with namespace %s", ns)
	}
	chaosList := chaosListInterface.ListChaos()
	if len(chaosList) == 0 {
		return nil, nil, fmt.Errorf("no chaos is found, please check your input")
	}

	var retList []runtime.Object
	var retNameList []string
	for _, ch := range chaosList {
		if chaosName == "" || chaosName == ch.Name {
			chaos, err := getChaos(ctx, chaosType, ch.Name, ns, c)
			if err != nil {
				return nil, nil, err
			}
			retList = append(retList, chaos)
			retNameList = append(retNameList, ch.Name)
		}
	}
	if len(retList) == 0 {
		return nil, nil, fmt.Errorf("no chaos is found, please check your input")
	}

	return retList, retNameList, nil
}

func getChaos(ctx context.Context, chaosType string, chaosName string, ns string, c client.Client) (runtime.Object, error) {
	allKinds := v1alpha1.AllKinds()
	chaos := allKinds[chaosType].Chaos
	objectKey := client.ObjectKey{
		Namespace: ns,
		Name:      chaosName,
	}
	if err := c.Get(ctx, objectKey, chaos); err != nil {
		return nil, errors.Wrapf(err, "failed to get chaos %s", chaosName)
	}
	return chaos, nil
}

// GetPidFromPS returns pid-command pairs
func GetPidFromPS(ctx context.Context, pod v1.Pod, daemon v1.Pod, c *kubernetes.Clientset) ([]string, []string, error) {
	cmd := "ps"
	out, err := ExecBypass(ctx, pod, daemon, cmd, c)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "run command %s failed", cmd)
	}
	outLines := strings.Split(string(out), "\n")
	if len(outLines) < 2 {
		return nil, nil, fmt.Errorf("ps returns empty")
	}
	titles := strings.Fields(outLines[0])
	var pidColumn, cmdColumn int
	for i, t := range titles {
		if t == "PID" {
			pidColumn = i
		}
		if t == "COMMAND" || t == "CMD" {
			cmdColumn = i
		}
	}
	if pidColumn == 0 && cmdColumn == 0 {
		return nil, nil, fmt.Errorf("parsing ps error: could not get PID and COMMAND column")
	}
	var pids, commands []string
	for _, line := range outLines[1:] {
		item := strings.Fields(line)
		// break when got empty line
		if len(item) == 0 {
			break
		}
		pids = append(pids, item[pidColumn])
		commands = append(commands, item[cmdColumn])
	}
	return pids, commands, nil
}

// GetPidFromPod returns pid given containerd ID in pod
func GetPidFromPod(ctx context.Context, pod v1.Pod, daemon v1.Pod) (uint32, error) {
	pfCancel, localPort, err := forwardPorts(ctx, daemon, uint16(ctrlconfig.ControllerCfg.ChaosDaemonPort))
	if err != nil {
		return 0, errors.Wrapf(err, "forward ports for daemon pod %s/%s failed", daemon.Namespace, daemon.Name)
	}
	L().WithName("GetPidFromPod").V(4).Info(fmt.Sprintf("port forwarding 127.0.0.1:%d -> pod/%s/%s:%d", localPort, daemon.Namespace, daemon.Name, ctrlconfig.ControllerCfg.ChaosDaemonPort))

	defer func() {
		pfCancel()
	}()

	// TODO: support specify the cert file or get cert file automatically
	daemonClient, err := NewChaosDaemonClientLocally(int(localPort), "", "", "")
	if err != nil {
		return 0, errors.Wrapf(err, "failed to create new chaos daemon client with local port %d", localPort)
	}
	defer daemonClient.Close()

	if len(pod.Status.ContainerStatuses) == 0 {
		return 0, fmt.Errorf("%s %s can't get the state of container", pod.Namespace, pod.Name)
	}

	res, err := daemonClient.ContainerGetPid(ctx, &pb.ContainerRequest{
		Action: &pb.ContainerAction{
			Action: pb.ContainerAction_GETPID,
		},
		ContainerId: pod.Status.ContainerStatuses[0].ContainerID,
	})
	if err != nil {
		return 0, errors.Wrapf(err, "failed get pid from pod %s/%s", pod.GetNamespace(), pod.GetName())
	}
	return res.Pid, nil
}

func forwardPorts(ctx context.Context, pod v1.Pod, port uint16) (context.CancelFunc, uint16, error) {
	commonRestClientGetter := NewCommonRestClientGetter()
	fw, err := portforward.NewPortForwarder(ctx, commonRestClientGetter, false)
	if err != nil {
		return nil, 0, errors.Wrap(err, "failed to create port forwarder")
	}
	_, localPort, pfCancel, err := portforward.ForwardOnePort(fw, pod.Namespace, pod.Name, port)
	return pfCancel, localPort, err
}

// Log print log of pod
func Log(pod v1.Pod, tail int64, c *kubernetes.Clientset) (string, error) {
	podLogOpts := v1.PodLogOptions{}
	//use negative tail to indicate no tail limit is needed
	if tail >= 0 {
		podLogOpts.TailLines = func(i int64) *int64 { return &i }(tail)
	}

	req := c.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &podLogOpts)
	podLogs, err := req.Stream()
	if err != nil {
		return "", errors.Wrapf(err, "failed to open log stream for pod %s/%s", pod.GetNamespace(), pod.GetName())
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", errors.Wrapf(err, "failed to copy information from podLogs to buf")
	}
	return buf.String(), nil
}

// CheckFailedMessage provide debug info and suggestions from failed message
func CheckFailedMessage(ctx context.Context, failedMessage string, daemons []v1.Pod, c *ClientSet) error {
	if strings.Contains(failedMessage, "rpc error: code = Unavailable desc = connection error") || strings.Contains(failedMessage, "connect: connection refused") {
		if err := checkConnForCtrlAndDaemon(ctx, daemons, c); err != nil {
			return fmt.Errorf("error occurs when check failed message: %s", err)
		}
	}
	return nil
}

func checkConnForCtrlAndDaemon(ctx context.Context, daemons []v1.Pod, c *ClientSet) error {
	ctrlSelector := v1alpha1.SelectorSpec{
		LabelSelectors: map[string]string{"app.kubernetes.io/component": "controller-manager"},
	}
	ctrlMgrs, err := selector.SelectPods(ctx, c.CtrlCli, c.CtrlCli, ctrlSelector, ctrlconfig.ControllerCfg.ClusterScoped, ctrlconfig.ControllerCfg.TargetNamespace, ctrlconfig.ControllerCfg.EnableFilterNamespace)
	if err != nil {
		return errors.Wrapf(err, "failed to select pod for controller-manager")
	}
	if len(ctrlMgrs) == 0 {
		return fmt.Errorf("could not found controller manager")
	}
	for _, daemon := range daemons {
		daemonIP := daemon.Status.PodIP
		cmd := fmt.Sprintf("ping -c 1 %s > /dev/null; echo $?", daemonIP)
		out, err := Exec(ctx, ctrlMgrs[0], cmd, c.KubeCli)
		if err != nil {
			return errors.Wrapf(err, "run command %s failed", cmd)
		}
		if string(out) == "0" {
			PrettyPrint(fmt.Sprintf("Connection between Controller-Manager and Daemon %s (ip address: %s) works well", daemon.Name, daemonIP), 0, Green)
		} else {
			PrettyPrint(fmt.Sprintf(`Connection between Controller-Manager and Daemon %s (ip address: %s) is blocked.
Please check network policy / firewall, or see FAQ on website`, daemon.Name, daemonIP), 0, Red)
		}

	}
	return nil
}

// NewChaosDaemonClientLocally would create ChaosDaemonClient in localhost
func NewChaosDaemonClientLocally(port int, caCert string, cert string, key string) (daemonClient.ChaosDaemonClientInterface, error) {
	if cli := mock.On("MockChaosDaemonClient"); cli != nil {
		return cli.(daemonClient.ChaosDaemonClientInterface), nil
	}
	if err := mock.On("NewChaosDaemonClientError"); err != nil {
		return nil, err.(error)
	}

	cc, err := grpcUtils.CreateGrpcConnection("localhost", port, caCert, cert, key)
	if err != nil {
		return nil, err
	}
	return daemonClient.New(cc), nil
}
