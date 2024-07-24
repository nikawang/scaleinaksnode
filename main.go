package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/profiles/latest/containerservice/mgmt/containerservice"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-03-01/compute"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/Azure/go-autorest/autorest/to"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/drain"
)

var nodesToDeleteWatch = make(map[string]bool)
var mutexWatch sync.Mutex // 使用互斥锁来保证 map 的并发安全
var nodesToCheckCycle = make(map[string]bool)

func main() {

	stopCh := setupSignalHandler()
	// Initialize Kubernetes client
	clientset := getKubernetesClient()
	// Initialize Azure VMSS client
	vmssClient, aksClient := getAzClient()

	coolDownStr := os.Getenv("COOLDOWN")
	coolDown, err := strconv.Atoi(coolDownStr)
	if err != nil {
		fmt.Printf("Error converting COOLDOWN to int: %s", err)
		return
	}

	// Start watching nodes
	go watchNodes(clientset, vmssClient, stopCh)

	//get flag from env
	is_scheduled_check_flag_str := os.Getenv("SCHEDULED_CHECK_FALG")
	is_scheduled_check_flag, err := strconv.ParseBool(is_scheduled_check_flag_str)
	if err != nil {
		fmt.Printf("Error converting SCHEDULED_CHECK_FALG to bool: %s", err)
		is_scheduled_check_flag = true
	}
	if is_scheduled_check_flag {
		go func() {
			for {
				select {
				case <-stopCh:
					log.Println("Shutting down node watcher...")
					return
				case <-time.After(time.Duration(coolDown) * 2 * time.Second):
					err := checkAndDeleteNodes(clientset, vmssClient, aksClient)
					if err != nil {
						log.Printf("Error checking and deleting nodes: %v\n", err)
					}
				}
			}
		}()
	}

	// Wait for stop signal
	<-stopCh
	fmt.Println("Shutting down node watcher...")
}

func checkAndDeleteNodes(clientset *kubernetes.Clientset, vmssClient compute.VirtualMachineScaleSetVMsClient, aksClient containerservice.AgentPoolsClient) error {
	// 从环境变量获取参数
	labelKey := os.Getenv("LABEL_KEY")
	labelValue := os.Getenv("LABEL_VALUE")
	vmssRG := os.Getenv("VMSS_RG")
	nodepool := os.Getenv("FOCUS_NODEPOOL")
	namespace := os.Getenv("NAMESPACE")
	labelSelector := "agentpool=" + nodepool
	// List all nodes
	nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}
	currentCheck := make(map[string]bool)
	for _, node := range nodes.Items {
		// Check if the node has the specified label
		if val, ok := node.Labels[labelKey]; ok && val == labelValue {
			continue
		}
		fmt.Printf("Start to check node %s \n", node.Name)
		// Check if there are any pods with the specified label on the node
		pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", labelKey, labelValue),
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
		})
		if err != nil {
			return fmt.Errorf("failed to list pods on node %s: %v", node.Name, err)
		}

		if len(pods.Items) == 0 {
			if nodesToCheckCycle[node.Name] {
				// Taint the node to prevent new pods from being scheduled on it
				// err = AddTaintToNode(clientset, node.Name, &corev1.Taint{
				// 	Key:    "kubernetes.io/casanti",
				// 	Value:  "noscalein",
				// 	Effect: corev1.TaintEffectNoSchedule,
				// })
				// if err != nil {
				// 	return fmt.Errorf("failed to taint node %s: %v", node.Name, err)
				// }

				// Drain the node
				err = DrainNode(clientset, node.Name)
				if err != nil {
					return fmt.Errorf("failed to drain node %s: %v", node.Name, err)
				}

				// Convert AKS node name to VMSS instance ID
				vmssName, instanceID, err := convertNodeNameToVMSS(node.Name)
				if err != nil {
					return fmt.Errorf("failed to convert node name to VMSS instance ID: %v", err)
				}
				err = deleteNode(clientset, node.Name)
				if err != nil {
					return fmt.Errorf("Error deleting node %s from Kubernetes: %v\n", node.Name, err)
				}
				// Delete the VMSS instance
				fmt.Printf("Deleting VMSS instance with ID: %s\n", instanceID)
				_, err = vmssClient.Delete(context.TODO(), vmssRG, vmssName, instanceID, nil)
				if err != nil {
					return fmt.Errorf("Error deleting VMSS instance: %v\n", err)
				}
				delete(nodesToCheckCycle, node.Name)
				log.Printf("Deleted VMSS instance %s and node %s\n", instanceID, node.Name)
			} else {
				currentCheck[node.Name] = true
			}

		} else {
			// 如果节点上有Pod，则确保它不会在下一个周期被错误地删除
			delete(nodesToCheckCycle, node.Name)
		}

	}
	// nodesToCheckCycle = currentCheck
	for nodeName := range currentCheck {
		nodesToCheckCycle[nodeName] = true
	}

	// 清除不再存在的节点
	for nodeName := range nodesToCheckCycle {
		if _, exists := currentCheck[nodeName]; !exists {
			delete(nodesToCheckCycle, nodeName)
		}
	}
	//print nodesToCheckCycle
	fmt.Println("nodesToCheckCycle at this round:", nodesToCheckCycle)
	is_fakenp_check_flag_str := os.Getenv("FAKENP_FLAG")
	is_fakenp_check_flag, err := strconv.ParseBool(is_fakenp_check_flag_str)
	if err != nil {
		fmt.Printf("Error converting FAKENP_FLAG to bool: %s", err)
		is_fakenp_check_flag = true
	}
	if is_fakenp_check_flag {
		ctx := context.Background()
		aksRG := os.Getenv("AKS_RG")
		aksClusterName := os.Getenv("AKS_NAME")
		fakeNP := os.Getenv("FAKE_NODEPOOL")
		rand.Seed(time.Now().UnixNano())

		// 生成0到10之间的随机数作为最大数量
		maxCount := int32(rand.Intn(21))
		agentPool, err := aksClient.Get(ctx, aksRG, aksClusterName, fakeNP)
		if err != nil {
			log.Fatalf("Failed to get agent pool: %v", err)
		}
		zero := int32(0)
		// 更新最小和最大数量
		agentPool.Count = &zero
		agentPool.MinCount = &zero
		agentPool.MaxCount = &maxCount
		agentPool.EnableAutoScaling = to.BoolPtr(true) // 启用自动缩放

		// 更新Node Pool
		_, err = aksClient.CreateOrUpdate(ctx, aksRG, aksClusterName, fakeNP, agentPool)
		if err != nil {
			log.Fatalf("Failed to update agent pool: %v", err)
		}
		fmt.Printf("Updated agent pool %s with max count %d\n", fakeNP, maxCount)
	}
	return nil
}

func setupSignalHandler() chan os.Signal {
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	return stopCh
}

func getKubernetesClient() *kubernetes.Clientset {
	var config *rest.Config
	var err error

	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		// 使用环境变量指定的 kubeconfig 文件
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else if home := homeDir(); home != "" {
		kubeconfig := filepath.Join(home, ".kube", "config")
		if _, err := os.Stat(kubeconfig); err == nil {
			// 如果 .kube/config 存在，则使用该配置
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		} else {
			// 如果 .kube/config 不存在，检查是否在集群内部运行
			if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
				config, err = rest.InClusterConfig()
			}
		}
	} else {
		panic(fmt.Errorf("Error finding kubeconfig or service account token"))
	}

	if err != nil {
		panic(fmt.Errorf("Error creating kubernetes client config: %s", err))
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(fmt.Errorf("Error creating kubernetes client: %s", err))
	}

	return clientset
}

func getAzClient() (compute.VirtualMachineScaleSetVMsClient, containerservice.AgentPoolsClient) {
	authorizer, err := auth.NewAuthorizerFromEnvironment()
	if err != nil {
		panic(err.Error())
	}
	vmssClient := compute.NewVirtualMachineScaleSetVMsClient(os.Getenv("AZURE_SUBSCRIPTION_ID"))
	vmssClient.Authorizer = authorizer

	aksClient := containerservice.NewAgentPoolsClient(os.Getenv("AZURE_SUBSCRIPTION_ID"))
	aksClient.Authorizer = authorizer
	return vmssClient, aksClient
}

func watchNodes(clientset *kubernetes.Clientset, vmssClient compute.VirtualMachineScaleSetVMsClient, stopCh chan os.Signal) {
	fmt.Println("Starting to watch nodes and pods...")

	// 从环境变量获取参数
	labelKey := os.Getenv("LABEL_KEY")
	labelValue := os.Getenv("LABEL_VALUE")
	vmssRG := os.Getenv("VMSS_RG")
	namespace := os.Getenv("NAMESPACE")
	// vmssName := os.Getenv("VMSS_NAME")
	fmt.Println("Label key:", labelKey)
	fmt.Println("Label value:", labelValue)
	fmt.Println("Resource group:", vmssRG)
	// fmt.Println("VMSS name:", vmssName)

	watcher, err := clientset.CoreV1().Pods(namespace).Watch(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	defer watcher.Stop()
	ch := watcher.ResultChan()

	// 监视节点状态
	nodepool := os.Getenv("FOCUS_NODEPOOL")
	labelSelector := "agentpool=" + nodepool
	nodeWatcher, err := clientset.CoreV1().Nodes().Watch(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})

	// nodeWatcher, err := clientset.CoreV1().Nodes().Watch(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	defer nodeWatcher.Stop()

	nodeCh := nodeWatcher.ResultChan()

	for {
		select {
		case event := <-ch:
			// fmt.Println("Received pod event:", event.Type)
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			// 只关注Pod状态为"Succeeded"或"Failed"的事件
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				continue
			}

			// if event.Type == watch.Deleted || event.Type == watch.Modified {
			if event.Type == watch.Modified {
				nodeName := pod.Spec.NodeName
				// 检查Node上是否还有其他具有特定标签的Pod处于Running状态
				pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: labelKey + "=" + labelValue,
					FieldSelector: "spec.nodeName=" + nodeName,
				})

				if err != nil {
					fmt.Printf("Error getting pods for node %s: %v\n", nodeName, err)
					continue
				}

				hasRunningOrPendingPods := false
				for _, pod := range pods.Items {
					if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
						hasRunningOrPendingPods = true
						break
					}
				}
				if !hasRunningOrPendingPods {
					// 如果没有其他Running状态的Pod，则删除Node
					// fmt.Printf("No running or pending pods found on node %s, deleting node...\n", nodeName)
					if len(pods.Items) == 0 {
						mutexWatch.Lock()
						if _, exists := nodesToDeleteWatch[nodeName]; !exists {
							// 如果节点不在删除队列中，则添加到队列并启动 goroutine
							nodesToDeleteWatch[nodeName] = true
							//print nodesToDelete
							fmt.Println("nodesToDelete:", nodesToDeleteWatch)
							mutexWatch.Unlock()

							// 给Node添加NoSchedule taint
							go func(nodeName string) {
								// 等待两分钟
								coolDownStr := os.Getenv("COOLDOWN")
								coolDown, err := strconv.Atoi(coolDownStr)
								if err != nil {
									fmt.Printf("Error converting COOLDOWN to int: %s", err)
									return
								}

								time.Sleep(time.Duration(coolDown) * time.Second)
								// 再次检查Node上是否还有其他具有特定标签的Pod处于Running状态
								pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
									LabelSelector: labelKey + "=" + labelValue,
									FieldSelector: "spec.nodeName=" + nodeName,
								})

								if err != nil {
									fmt.Printf("Error getting pods for node %s after delay: %v\n", nodeName, err)
									return
								}

								if len(pods.Items) == 0 {
									// 如果两分钟后仍然没有Running状态的Pod，则删除Node
									fmt.Printf("No running or pending pods found on node %s after %s minutes, deleting node...\n", nodeName, coolDownStr)
									mutexWatch.Lock()
									// err := AddTaintToNode(clientset, nodeName, &corev1.Taint{
									// 	Key:    "kubernetes.io/casanti",
									// 	Value:  "noscalein",
									// 	Effect: corev1.TaintEffectNoSchedule,
									// })
									// if err != nil {
									// 	fmt.Printf("Error adding taint to node %s: %v\n", nodeName, err)
									// 	mutexWatch.Unlock()
									// 	return
									// }

									// Drain Node
									err = DrainNode(clientset, nodeName)
									if err != nil {
										fmt.Printf("Error draining node %s: %v\n", nodeName, err)
										mutexWatch.Unlock()
										return
									}

									// 转换Node名称到VMSS实例ID
									vmssName, instanceID, err := convertNodeNameToVMSS(nodeName)
									fmt.Println("vmssName:", vmssName, "instanceID:", instanceID)
									if err != nil {
										fmt.Printf("Error converting node name to VMSS name and instance ID: %v\n", err)
										mutexWatch.Unlock()
										return
									}

									// 从 Kubernetes 集群中删除节点
									err = deleteNode(clientset, nodeName)
									if err != nil {
										fmt.Printf("Error deleting node %s from Kubernetes: %v\n", nodeName, err)
										mutexWatch.Unlock()
										return
									}
									fmt.Printf("Node %s deleted from Kubernetes\n", nodeName)

									// 删除VMSS实例
									fmt.Printf("Deleting VMSS instance with ID: %s\n", instanceID)
									_, err = vmssClient.Delete(context.TODO(), vmssRG, vmssName, instanceID, nil)
									if err != nil {
										fmt.Printf("Error deleting VMSS instance: %v\n", err)
										mutexWatch.Unlock()
										return
									}
									delete(nodesToDeleteWatch, nodeName) // 删除成功后，从删除队列中移除节点
									fmt.Println("nodesToDelete:", nodesToDeleteWatch)
									mutexWatch.Unlock()
								} else {
									mutexWatch.Lock()
									delete(nodesToDeleteWatch, nodeName) // 如果节点上有Pods，从删除队列中移除节点
									fmt.Println("nodesToDelete:", nodesToDeleteWatch)
									mutexWatch.Unlock()
								}
							}(nodeName)
						} else {
							mutexWatch.Unlock()
						}
					}
				}
			}
		case nodeEvent := <-nodeCh:
			// fmt.Println("Received node event:", nodeEvent.Type)
			node, ok := nodeEvent.Object.(*corev1.Node)
			if !ok {
				continue
			}
			if isNodeReady(node) {
				// 检查节点上是否已经有了对应的 Place Holder pod
				exists, err := doesPlaceHolderPodExistOnNode(clientset, node.Name)
				if err != nil {
					fmt.Printf("Error checking for existing Place Holder pod on node %s: %v\n", node.Name, err)
					continue
				}
				if !exists {
					// 在 Ready 节点上运行 Place Holder pod
					err := runNginxPodOnNode(clientset, node.Name)
					if err != nil {
						fmt.Printf("Error running Place Holder pod on node %s: %v\n", node.Name, err)
					}
				} else {
					// fmt.Printf("Place Holder pod already exists on node %s, skipping creation\n", node.Name)
				}
			}
		case <-stopCh:
			fmt.Println("Received stop signal, cleaning up...")
			watcher.Stop()
			nodeWatcher.Stop()
			fmt.Println("Node and pod watchers stopped.")
			return
		}
	}

}

func deleteNode(clientset *kubernetes.Clientset, nodeName string) error {
	// 设置删除选项，例如设置宽限期为0秒
	deletePolicy := metav1.DeleteOptions{
		GracePeriodSeconds: new(int64), // 0秒宽限期
	}
	// 删除节点
	if err := clientset.CoreV1().Nodes().Delete(context.TODO(), nodeName, deletePolicy); err != nil {
		return fmt.Errorf("failed to delete node %s: %v", nodeName, err)
	}
	return nil
}

func doesPlaceHolderPodExistOnNode(clientset *kubernetes.Clientset, nodeName string) (bool, error) {
	pods, err := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "casanti=placeholder", // 假设您的 Place Holder pod 有一个 'app=nginx' 的标签
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName == nodeName && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			// 如果节点上有一个 Place Holder pod 并且它不是在 Succeeded 或 Failed 状态，我们认为 Pod 已经存在
			return true, nil
		}
	}
	return false, nil
}

func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// DrainNode 使用 kubectl 的 drain 包来驱逐节点上的 Pod
func DrainNode(clientset *kubernetes.Clientset, nodeName string) error {
	fmt.Printf("Draining node: %s\n", nodeName)

	node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	drainer := &drain.Helper{
		Client:              clientset,
		Force:               true,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		GracePeriodSeconds:  -1,
		Timeout:             30 * time.Second,
		Out:                 os.Stdout,
		ErrOut:              os.Stderr,
	}

	return drain.RunNodeDrain(drainer, node.Name)
}

// AddTaintToNode 使用 client-go 库给节点添加 taint
// func AddTaintToNode(clientset *kubernetes.Clientset, nodeName string, taint *corev1.Taint) error {
// 	fmt.Printf("Adding taint to node: %s\n", nodeName)
// 	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
// 		node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
// 		if err != nil {
// 			return err
// 		}

// 		// 检查 taint 是否已经存在
// 		for _, existingTaint := range node.Spec.Taints {
// 			if existingTaint.Key == taint.Key && existingTaint.Value == taint.Value && existingTaint.Effect == taint.Effect {
// 				// Taint 已存在，无需添加
// 				fmt.Printf("Taint has been existed to node: %s\n", nodeName)
// 				return nil
// 			}
// 		}

// 		// 添加 taint
// 		node.Spec.Taints = append(node.Spec.Taints, *taint)
// 		// _, err = clientset.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})

// 		fmt.Printf("Taint added to node: %s\n", nodeName)
// 		return err
// 	})
// }

func convertNodeNameToVMSS(nodeName string) (vmssName string, instanceID string, err error) {
	// 使用 "vmss" 作为分隔符来切割字符串
	parts := strings.Split(nodeName, "vmss")
	if len(parts) != 2 {
		err = fmt.Errorf("invalid node name format")
		return
	}

	// VMSS 名称是第一个部分加上 "vmss"
	vmssName = parts[0] + "vmss"
	// 36 进制的尾数是第二个部分
	base36ID := parts[1]

	// 将 36 进制的尾数转换为 10 进制
	numericID, convErr := strconv.ParseUint(base36ID, 36, 64)
	if convErr != nil {
		err = fmt.Errorf("failed to convert base36 to base10: %v", convErr)
		return
	}

	// 构造 VMSS 实例 ID
	instanceID = fmt.Sprintf("%d", numericID)
	return
}

func runNginxPodOnNode(clientset *kubernetes.Clientset, nodeName string) error {
	fmt.Printf("Creating Place Holder pod on node: %s\n", nodeName)
	labels := map[string]string{
		"casanti": "placeholder", // 例如，添加一个标签 app=nginx
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "placeholder-" + nodeName,
			Namespace: "default",
			Labels:    labels,
			Annotations: map[string]string{
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx",
				},
			},
			NodeName: nodeName,
			Tolerations: []corev1.Toleration{
				{
					Key:      "kubernetes.azure.com/scalesetpriority",
					Operator: corev1.TolerationOpEqual,
					Value:    "spot",
					Effect:   corev1.TaintEffectNoSchedule,
				},
			},
		},
	}

	_, err := clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("Failed to create Place Holder pod on node %s: %v\n", nodeName, err)
	} else {
		fmt.Printf("Place Holder pod created on node: %s\n", nodeName)
	}
	return err
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}
