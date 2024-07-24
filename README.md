### Replace the Scale IN for an autoscaling AKS nodepool

#### Introduction
Creating a Kubernetes client application that watches for Pods with a specific label on AKS nodes. If a node does not have any Pods with the specified label, the application should delete that node and the specific instance of Azure VMSS. 

1. Watch the state changes of the target Pods on AKS nodes (based on Pod label and namespace) to determine if the corresponding Pods still exist on the node. If not, the node is marked for deletion. Then, wait for a time period to check again if the node still lacks the corresponding Pods. If it does, delete the node.

2. Start another goroutine to periodically check the nodes in the corresponding node pool to see if they still have the specified Pods. If not, the nodes are marked for deletion. Then, wait for another time period to check again if the nodes still lack the corresponding Pods. If they do, delete the nodes.

3. Node deletion logic:
   - First, perform a taint&drain, then delete the Kubernetes node.
   - Convert the AKS node name (with a base-36 suffix) to an AKS VMSS instance ID (with a base-10 suffix).
   - Delete the AKS VMSS instance.

4. To prevent potential state inconsistencies in the Cluster Autoscaler (CAS) after manually deleting a node or a VMSS instance ID, periodically update the maximum count value of a different node pool that has zero nodes. This action prompts the CAS to synchronize its state accordingly.

#### Details
Your program implements a Kubernetes client that monitors Pods and Nodes in an Azure Kubernetes Service (AKS) cluster and automatically deletes Nodes from the AKS cluster under specific conditions. The main logic of the program is divided into several parts:

1. **Initialize Kubernetes and Azure Clients**:
   - The `getKubernetesClient` function creates and returns a Kubernetes client instance for interacting with the Kubernetes API.
   - The `getAzClient` function creates and returns an Azure VMSS (Virtual Machine Scale Set) client instance for interacting with the Azure Resource Manager.

2. **Monitor Pod Changes**:
   - The `watchNodes` function starts a goroutine to monitor changes in Pods. When a Pod's status changes to `Succeeded` or `Failed`, the program checks if there are other Pods in `Running` or `Pending` status on the same Node. If not, the Node is marked for deletion.

3. **Node Deletion Logic**:
   - If a Node is marked for deletion, the program waits for a cooldown period (specified by the `COOLDOWN` environment variable) and then checks the Node again for Pods. If there are still no Pods, the program performs the following steps:
     - Drains the Node using the `DrainNode` function to safely evict all Pods from the Node.
     - Deletes the Node from the Kubernetes cluster using the `deleteNode` function.
     - Converts the AKS Node name to a VMSS instance ID using the `convertNodeNameToVMSS` function.
     - Deletes the corresponding VMSS instance using the Azure VMSS client.

4. **Periodic Node Check**:
   - There is a periodically running goroutine in the program that checks if Nodes in the NodePool are not running Pods with the specified label. If a Node does not run Pods with the specified label for two consecutive check cycles, it will be deleted.

5. **CAS State Synchronization**:
   - To prevent potential state inconsistencies in the Cluster Autoscaler (CAS) after manual Node or VMSS instance deletions, the program can periodically update the maximum value of a fake NodePool to prompt CAS to synchronize its state.

6. **Signal Handling**:
   - The `setupSignalHandler` function sets up signal handling to gracefully shut down the program when a termination signal is received.

The program's operation is configured through environment variables, which include:
- `COOLDOWN`: The cooldown period to wait before deleting a Node.
- `LABEL_KEY` and `LABEL_VALUE`: The label key-value pair used to identify whether a Pod should be retained on a Node.
- `VMSS_RG`: The VMSS resource group name.
- `NAMESPACE`: The Kubernetes namespace.
- `FOCUS_NODEPOOL`: The name of the NodePool to focus on.
- `SCHEDULED_CHECK_FALG`: A flag to enable periodic Node checks.
- `FAKENP_FLAG`: A flag to enable CAS state synchronization.
- `AKS_RG`, `AKS_NAME`, `FAKE_NODEPOOL`: The AKS resource group, AKS cluster name, and fake NodePool name used for CAS state synchronization.

The program manages the automatic scaling down of Nodes in the AKS cluster by listening for changes in Pods and Nodes and by periodically checking the state of Nodes.
