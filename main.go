package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
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
	"github.com/Azure/go-autorest/autorest"
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
var url = fmt.Sprintf("https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachineScaleSets/%s/delete?api-version=2024-03-01",
	os.Getenv("AZURE_SUBSCRIPTION_ID"), os.Getenv("VMSS_RG"), os.Getenv("VMSS_NAME"))

func main() {
	log.Printf("Hello")
	stopCh := setupSignalHandler()
	// Initialize Kubernetes client
	clientset := getKubernetesClient()
	// Initialize Azure VMSS client
	vmssClient, aksClient := getAzClient()

	coolDownStr := os.Getenv("COOLDOWN")
	coolDown, err := strconv.Atoi(coolDownStr)
	if err != nil {
		log.Printf("Error converting COOLDOWN to int: %s\n", err)
		return
	}

	fakecoolDownStr := os.Getenv("FAKENP_COOLDOWN")
	fakecoolDown, err := strconv.Atoi(fakecoolDownStr)
	if err != nil {
		log.Printf("Error converting FAKENP_COOLDOWN to int: %s\n", err)
		return
	}
	if fakecoolDown == 0 {
		fakecoolDown = 180
	}

	log.Printf("FAKENP_COOLDOWN value: %d\n", fakecoolDown)

	// Start watching nodes
	go watchK8SEvents(clientset, vmssClient, stopCh)

	go processNodesbyEvents(clientset, vmssClient)

	//get flag from env
	is_scheduled_check_flag_str := os.Getenv("SCHEDULED_CHECK_FALG")
	is_scheduled_check_flag, err := strconv.ParseBool(is_scheduled_check_flag_str)
	if err != nil {
		log.Printf("Error converting SCHEDULED_CHECK_FALG to bool: %s\n", err)
		is_scheduled_check_flag = true
	}
	if is_scheduled_check_flag {
		log.Println("On Scheduled: Starting to clean the useless nodes...")
		go func() {
			for {
				select {
				case <-stopCh:
					log.Println("shutting down node watcher...")
					return
				case <-time.After(time.Duration(coolDown) * 2 * time.Second):
					err := checkAndDeleteNodes(clientset, vmssClient, aksClient)
					if err != nil {
						log.Printf("Error checking and deleting nodes: %v\n", err)
					}
				case <-time.After(time.Duration(fakecoolDown) * time.Second):
					updateFakeNP(aksClient, os.Getenv("AKS_RG"), os.Getenv("AKS_NAME"), os.Getenv("FAKE_NODEPOOL"))
				}
			}
		}()
	}

	// Wait for stop signal
	<-stopCh
	log.Println("shutting down node watcher...")
}

func DeleteVMInstances(vmssClient compute.VirtualMachineScaleSetVMsClient, instanceIDs []string) error {
	// 创建一个 authorizer
	authorizer := vmssClient.Authorizer
	// 创建请求体
	body := struct {
		InstanceIds []string `json:"instanceIds"`
	}{
		InstanceIds: instanceIDs,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}

	// 构建 HTTP 请求
	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// 使用 authorizer 为请求添加授权头部
	preparer := autorest.CreatePreparer(
		authorizer.WithAuthorization(),
	)
	req, err = preparer.Prepare(req)
	if err != nil {
		return err
	}

	// 创建一个 HTTP 客户端并发送请求
	client := autorest.NewClientWithUserAgent("Azure-Go-SDK/2024-03-01")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 检查响应状态码
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		log.Printf("API call to delete VM instances returned status: %s", resp.Status)
		return fmt.Errorf("API call to delete VM instances returned status: %s", resp.Status)
	}

	// 处理响应
	// log.Printf("VMSS DeleteResponse status: %s\n", resp.Status)

	return nil
}

func processNodesbyEvents(clientset *kubernetes.Clientset, vmssClient compute.VirtualMachineScaleSetVMsClient) {
	labelKey := os.Getenv("LABEL_KEY")
	labelValue := os.Getenv("LABEL_VALUE")
	vmssRG := os.Getenv("VMSS_RG")
	namespace := os.Getenv("NAMESPACE")
	coolDownStr := os.Getenv("COOLDOWN")
	coolDown, _ := strconv.Atoi(coolDownStr)
	isDeleteInBatchStr := os.Getenv("DELETE_IN_BATCH")
	isDeleteInBatch, _ := strconv.ParseBool(isDeleteInBatchStr)

	// vmssName := os.Getenv("VMSS_NAME")
	for {
		vmssInstances := []string{}
		mutexWatch.Lock()
		// nodesToDeleteWatch 赋值给新的 map，以便在迭代时删除元素
		nodesToDelete := make(map[string]bool)
		for k, v := range nodesToDeleteWatch {
			nodesToDelete[k] = v
		}
		mutexWatch.Unlock()
		time.Sleep(1 * time.Duration(coolDown) * time.Second)
		for nodeName := range nodesToDelete {
			// mutexWatch.Lock()
			// time.Sleep(1 * time.Duration(coolDown) * time.Second)
			pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: labelKey + "=" + labelValue,
				FieldSelector: "spec.nodeName=" + nodeName,
			})
			// mutexWatch.Unlock()
			if err != nil {
				log.Printf("Process Pod Event: Error getting pods for node %s after delay: %v\n", nodeName, err)
				return
			}

			var podList []corev1.Pod
			for _, pod := range pods.Items {

				if string(pod.Status.Phase) == "Running" || string(pod.Status.Phase) == "Pending" {
					podList = append(podList, pod)
					// hasRunningOrPendingPods = true
					break
				} else {
					log.Printf("Process Pod Event: Status Check\t Node %s Pod  %s status %s ... \n", nodeName, pod.Name, pod.Status.Phase)
				}
			}

			if len(podList) == 0 {
				// 如果两分钟后仍然没有Running状态的Pod，则删除Node
				log.Printf("Process Pod Event: No running or pending pods found on node %s after %s seconds, confirm to delete node...\n", nodeName, coolDownStr)
				// mutexWatch.Lock()

				// Drain Node
				log.Printf("Process Pod Event: Cordon node: %s\n", nodeName)
				err = CordonNode(clientset, nodeName)
				log.Printf("Process Pod Event: Cordon node: %s Completed \n", nodeName)
				if err != nil {
					log.Printf("Process Pod Event: Error Cordon node %s: %v\n", nodeName, err)
					// mutexWatch.Unlock()
					continue
				}

				// 转换Node名称到VMSS实例ID
				vmssName, instanceID, err := convertNodeNameToVMSS(nodeName)
				log.Printf("Pod Event: vmssName: %s, instanceID: %s\n", vmssName, instanceID)
				if err != nil {
					log.Printf("Pod Event: Error converting node name to VMSS name and instance ID: %v\n", err)
					// mutexWatch.Unlock()
					continue
				}
				vmssInstances = append(vmssInstances, instanceID)

				// 从 Kubernetes 集群中删除节点
				log.Printf("Pod Event: Deleting K8S node : %s\n", nodeName)
				err = deleteNode(clientset, nodeName)
				if err != nil {
					log.Printf("Pod Event: Error deleting node %s from Kubernetes: %v\n", nodeName, err)
					// mutexWatch.Unlock()
					continue
				}
				log.Printf("Pod Event: Node %s deleted from Kubernetes\n", nodeName)

				// 删除VMSS实例
				if !isDeleteInBatch {
					log.Printf("Pod Event: Deleting VMSS instance with ID: %s\n", instanceID)
					_, err = vmssClient.Delete(context.TODO(), vmssRG, vmssName, instanceID, nil)
					if err != nil {
						log.Printf("Pod Event: Error deleting VMSS instance: %v\n", err)
						// mutexWatch.Unlock()
						continue
					}
				}
				mutexWatch.Lock()
				delete(nodesToDeleteWatch, nodeName) // 删除成功后，从删除队列中移除节点
				log.Printf("Pod Event: Candidate delete node list: %v\n", nodesToDeleteWatch)
				mutexWatch.Unlock()
			} else {
				podNames := ""
				for _, pod := range podList {
					podNames += pod.Name + " "
				}
				mutexWatch.Lock()
				delete(nodesToDeleteWatch, nodeName) // 如果节点上有Pods，从删除队列中移除节点
				log.Printf("Process Pod Event: There're Pods %s\t  on node %s, remove node name from candidate deleting node\n", podNames, nodeName)
				log.Printf("Process Pod Event: candidate delete node list: %v\n", nodesToDeleteWatch)
				mutexWatch.Unlock()
			}
			// }(nodeName)
		}
		defer func() {
			if isDeleteInBatch {
				if len(vmssInstances) > 0 {
					log.Printf("Process Pod Event: Batch Deleting VMSS instance with ID: %s\n", vmssInstances)
					DeleteVMInstances(vmssClient, vmssInstances)
				}
			}
		}()

		defer func() {
			if r := recover(); r != nil {
				log.Printf("Process Pod Event: Recovered from panic: %v\n", r)
			}
		}()

		// mutexWatch.Unlock()
	}
}

func checkAndDeleteNodes(clientset *kubernetes.Clientset, vmssClient compute.VirtualMachineScaleSetVMsClient, aksClient containerservice.AgentPoolsClient) error {
	// 从环境变量获取参数
	labelKey := os.Getenv("LABEL_KEY")
	labelValue := os.Getenv("LABEL_VALUE")
	vmssRG := os.Getenv("VMSS_RG")
	nodepool := os.Getenv("FOCUS_NODEPOOL")
	namespace := os.Getenv("NAMESPACE")
	labelSelector := "agentpool=" + nodepool
	isDeleteInBatchStr := os.Getenv("DELETE_IN_BATCH")
	isDeleteInBatch, _ := strconv.ParseBool(isDeleteInBatchStr)

	log.Printf("On Scheduled: Starting to collect pods from nodepool %s", labelSelector)
	// List all nodes
	nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})

	if err != nil {
		log.Printf("failed to list nodes: %v\n", err)
		return fmt.Errorf("failed to list nodes: %v", err)
	}
	currentCheck := make(map[string]bool)
	vmssInstances := []string{}
	for _, node := range nodes.Items {
		// Check if the node has the specified label
		// if val, ok := node.Labels[labelKey]; ok && val == labelValue {
		// 	continue
		// }
		log.Printf("On Scheduled: Start to check node %s \n", node.Name)
		// Check if there are any pods with the specified label on the node
		pods, err := clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", labelKey, labelValue),
			FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
		})

		// hasRunningOrPendingPods := false
		// add one podList to store the pods are not in Running or Pending status
		var podList []corev1.Pod
		for _, pod := range pods.Items {
			if string(pod.Status.Phase) == "Running" || string(pod.Status.Phase) == "Pending" {
				podList = append(podList, pod)
				// hasRunningOrPendingPods = true
				break
			}
		}

		// if hasRunningOrPendingPods {
		// 	// log.Printf("On Scheduled: There're Pods on node %s, skip\n", node.Name)
		// 	continue
		// }

		if err != nil {
			log.Printf("failed to list pods on node %s: %v\n", node.Name, err)
			return fmt.Errorf("failed to list pods on node %s: %v", node.Name, err)
		}

		if len(podList) == 0 {
			if nodesToCheckCycle[node.Name] {
				log.Printf("On Scheduled: No running or pending pods found on node %s seconds, confirm to delete node...\n", node.Name)
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
				log.Printf("On Scheduled: Cordon node: %s Completed \n", node.Name)
				err = CordonNode(clientset, node.Name)
				if err != nil {
					log.Printf("failed to Cordon node %s: %v\n", node.Name, err)
					return fmt.Errorf("failed to Cordon node %s: %v", node.Name, err)
				}

				// Convert AKS node name to VMSS instance ID
				vmssName, instanceID, err := convertNodeNameToVMSS(node.Name)
				vmssInstances = append(vmssInstances, instanceID)
				if err != nil {
					log.Printf("failed to convert node name to VMSS instance ID: %v\n", err)
					return fmt.Errorf("failed to convert node name to VMSS instance ID: %v", err)
				}
				log.Printf("On Scheduled: Deleting K8S Node: %s\n", node.Name)
				err = deleteNode(clientset, node.Name)
				if err != nil {
					log.Printf("Error deleting node %s from Kubernetes: %v\n", node.Name, err)
					return fmt.Errorf("error deleting node %s from Kubernetes: %v", node.Name, err)
				}
				// Delete the VMSS instance
				if !isDeleteInBatch {
					log.Printf("On Scheduled: Deleting VMSS instance with ID: %s\n", instanceID)
					_, err = vmssClient.Delete(context.TODO(), vmssRG, vmssName, instanceID, nil)
					if err != nil {
						log.Printf("Error deleting VMSS instance: %v\n", err)
						return fmt.Errorf("error deleting VMSS instance: %v", err)
					}
				}
				delete(nodesToCheckCycle, node.Name)
				// log.Printf("On Scheduled: Deleted VMSS instance %s and node %s\n", instanceID, node.Name)
			} else {
				currentCheck[node.Name] = true
			}

		} else {
			// 如果节点上有Pod，则确保它不会在下一个周期被错误地删除
			//print the name of pods in one line:
			podNames := ""
			for _, pod := range podList {
				podNames += pod.Name + " " + string(pod.Status.Phase) + " "
			}
			log.Printf("On Scheduled: There're Pods %s\t  on node %s\n", podNames, node.Name)
			log.Printf("On Scheduled: Remove node name from candidate deleting node %s\n", node.Name)
			delete(nodesToCheckCycle, node.Name)
		}

	}
	// isDeleteInBatch := os.Getenv("DELETE_IN_BATCH")
	defer func() {
		if isDeleteInBatch {
			if len(vmssInstances) > 0 {
				log.Printf("On Scheduled: Batch Deleting VMSS instance with ID: %s\n", vmssInstances)
				DeleteVMInstances(vmssClient, vmssInstances)
			}
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			log.Printf("On Scheduled: Recovered from panic: %v\n", r)
			// 可以在这里执行任何必要的清理操作
		}
	}()
	// nodesToCheckCycle = currentCheck
	for nodeName := range currentCheck {
		nodesToCheckCycle[nodeName] = true
	}

	// 清除不再存在的节点
	for nodeName := range nodesToCheckCycle {
		if _, exists := currentCheck[nodeName]; !exists {
			log.Printf("On Scheduled: Node name not existed in current candidate list.. %s\n", nodeName)
			log.Printf("On Scheduled: Remove node name from candidate deleting node %s\n", nodeName)
			delete(nodesToCheckCycle, nodeName)
		}
	}
	//print nodesToCheckCycle
	log.Printf("On Scheduled: nodesToCheckCycle at this round: %v\n", nodesToCheckCycle)

	return nil
}

func updateFakeNP(aksClient containerservice.AgentPoolsClient, aksRG string, aksClusterName string, fakeNP string) error {
	is_fakenp_check_flag_str := os.Getenv("FAKENP_FLAG")
	is_fakenp_check_flag, err := strconv.ParseBool(is_fakenp_check_flag_str)
	if err != nil {
		log.Printf("Error converting FAKENP_FLAG to bool: %s\n", err)
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
		log.Printf("Fake Nodepool: Updated agent pool %s with max count %d\n", fakeNP, maxCount)
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
			config, _ = clientcmd.BuildConfigFromFlags("", kubeconfig)
		} else {
			// 如果 .kube/config 不存在，检查是否在集群内部运行
			if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
				config, _ = rest.InClusterConfig()
			}
		}
	} else {
		log.Println("error finding kubeconfig or service account token")
		panic(fmt.Errorf("error finding kubeconfig or service account token"))
	}

	if err != nil {
		log.Printf("error creating kubernetes client config: %s\n", err)
		panic(fmt.Errorf("error creating kubernetes client config: %s", err))
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf("Error creating kubernetes client: %s\n", err)
		panic(fmt.Errorf("error creating kubernetes client: %s", err))
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

func watchK8SEvents(clientset *kubernetes.Clientset, vmssClient compute.VirtualMachineScaleSetVMsClient, stopCh chan os.Signal) {
	log.Println("Events: Starting to watch nodes and pods...")

	// 从环境变量获取参数
	labelKey := os.Getenv("LABEL_KEY")
	labelValue := os.Getenv("LABEL_VALUE")
	vmssRG := os.Getenv("VMSS_RG")
	namespace := os.Getenv("NAMESPACE")
	// vmssName := os.Getenv("VMSS_NAME")
	log.Printf("Label key %s\n", labelKey)
	log.Printf("Label value: %s\n", labelValue)
	log.Printf("Resource group: %s\n", vmssRG)
	// log.Printf("VMSS name:", vmssName)

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
			// log.Printf("Received pod event:", event.Type)
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				log.Println("Watch Pod Event: Pod watcher channel closed unexpectedly, exiting...")
				os.Exit(1)
			}

			if event.Type == watch.Modified && (pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded) {
				// log.Printf("Pod %s status %s ... \n", pod.Name, pod.Status.Phase)
				time.Sleep(1 * time.Duration(2) * time.Second)
				nodeName := pod.Spec.NodeName
				// 检查Node上是否还有其他具有特定标签的Pod处于Running状态
				pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
					LabelSelector: labelKey + "=" + labelValue,
					FieldSelector: "spec.nodeName=" + nodeName,
				})

				if err != nil {
					log.Printf("Watch Pod Event: Error getting pods for node %s: %v\n", nodeName, err)
					continue
				}

				var podList []corev1.Pod
				// hasRunningOrPendingPods := false
				for _, pod := range pods.Items {
					if string(pod.Status.Phase) == "Running" || string(pod.Status.Phase) == "Pending" {
						// hasRunningOrPendingPods = true
						podList = append(podList, pod)
						break
					}
				}
				// if !hasRunningOrPendingPods {
				// 如果没有其他Running状态的Pod，则删除Node
				// log.Printf("No running or pending pods found on node %s, deleting node...\n", nodeName)
				if len(podList) == 0 {
					log.Printf("Watch Pod Event: No running or pending pods found on node %s, consider to delete the node...\n", nodeName)
					mutexWatch.Lock()
					if _, exists := nodesToDeleteWatch[nodeName]; !exists {
						// 如果节点在删除队列中，则添加到队列并启动 goroutine
						// mutexWatch.Lock()
						nodesToDeleteWatch[nodeName] = true
						//print nodesToDelete
						log.Printf("Watch Pod Event: nodesToDelete: %v\n", nodesToDeleteWatch)

					} else {
						podName := ""
						for _, pod := range podList {
							podName += pod.Name + " "
						}

						log.Printf("Watch Pod Event: Node %s already in candidate delete node list, skip to add it...\n", nodeName)
					}
					mutexWatch.Unlock()

				}
				// }
			}
		case nodeEvent := <-nodeCh:
			log.Printf("Received node event: %s\n", nodeEvent.Type)
			node, ok := nodeEvent.Object.(*corev1.Node)
			if !ok {
				log.Println("nodeEvent: Node watcher channel closed unexpectedly, exiting...")
				os.Exit(1)
			}
			if isNodeReady(node) {
				// 检查节点上是否已经有了对应的 Place Holder pod
				exists, err := doesPlaceHolderPodExistOnNode(clientset, node.Name)
				if err != nil {
					log.Printf("nodeEvent: Error checking for existing Place Holder pod on node %s: %v\n", node.Name, err)
					continue
				}
				if !exists {
					// 在 Ready 节点上运行 Place Holder pod
					err := runPFPodOnNode(clientset, node.Name)
					if err != nil {
						log.Printf("nodeEvent: Error running Place Holder pod on node %s: %v\n", node.Name, err)
					}
				} else {
					// log.Printf("Place Holder pod already exists on node %s, skipping creation\n", node.Name)
				}
			} else {
				log.Printf("nodeEvent:  Node %s is not ready, skipping Place Holder pod creation\n", node.Name)
			}
		case <-stopCh:
			log.Println("Received stop signal, cleaning up...")
			watcher.Stop()
			nodeWatcher.Stop()
			log.Println("Node and pod watchers stopped.")
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
		log.Printf("failed to delete node %s: %v\n", nodeName, err)
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

// CordonNode 使用 kubectl 的 drain 包来驱逐节点上的 Pod
func CordonNode(clientset *kubernetes.Clientset, nodeName string) error {
	// log.Printf("On Scheduled: Draining node: %s\n", nodeName)
	is_cordonFlagStr := os.Getenv("CORDON_FLAG")
	is_cordonFlag, err := strconv.ParseBool(is_cordonFlagStr)
	if err != nil {
		log.Printf("Error converting CORDON_FLAG to bool: %s\n", err)
	}
	if is_cordonFlag {
		node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		drainer := &drain.Helper{
			Client:              clientset,
			Ctx:                 context.Background(),
			Force:               true,
			IgnoreAllDaemonSets: true,
			DeleteEmptyDirData:  true,
			GracePeriodSeconds:  -1,
			Timeout:             30 * time.Second,
			Out:                 os.Stdout,
			ErrOut:              os.Stderr,
		}

		// return drain.RunNodeDrain(drainer, node.Name)
		return drain.RunCordonOrUncordon(drainer, node, true)
	} else {
		return nil
	}
}

// AddTaintToNode 使用 client-go 库给节点添加 taint
// func AddTaintToNode(clientset *kubernetes.Clientset, nodeName string, taint *corev1.Taint) error {
// 	log.Printf("Adding taint to node: %s\n", nodeName)
// 	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
// 		node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
// 		if err != nil {
// 			return err
// 		}

// 		// 检查 taint 是否已经存在
// 		for _, existingTaint := range node.Spec.Taints {
// 			if existingTaint.Key == taint.Key && existingTaint.Value == taint.Value && existingTaint.Effect == taint.Effect {
// 				// Taint 已存在，无需添加
// 				log.Printf("Taint has been existed to node: %s\n", nodeName)
// 				return nil
// 			}
// 		}

// 		// 添加 taint
// 		node.Spec.Taints = append(node.Spec.Taints, *taint)
// 		// _, err = clientset.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})

// 		log.Printf("Taint added to node: %s\n", nodeName)
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
		log.Printf("failed to convert base36 to base10: %v\n", convErr)
		err = fmt.Errorf("failed to convert base36 to base10: %v", convErr)
		return
	}

	// 构造 VMSS 实例 ID
	instanceID = fmt.Sprintf("%d", numericID)
	return
}

func runPFPodOnNode(clientset *kubernetes.Clientset, nodeName string) error {
	log.Printf("Creating Place Holder pod on node: %s\n", nodeName)
	phImg := os.Getenv("PLACEHOLDER_IMG")
	if phImg == "" {
		phImg = "nginx"
	}
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
					Image: phImg,
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
		log.Printf("Failed to create Place Holder pod on node %s: %v\n", nodeName, err)
	} else {
		log.Printf("Place Holder pod created on node: %s\n", nodeName)
	}
	return err
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}
