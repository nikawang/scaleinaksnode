from azure.identity import DefaultAzureCredential
from azure.mgmt.compute import ComputeManagementClient
from kubernetes import client, config
from kubernetes.client.rest import ApiException
import os
import logging
import time
from azure.mgmt.compute.models import VirtualMachineScaleSetVMInstanceRequiredIDs


logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s %(levelname)s: %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)


def convert_node_name_to_vmss(node_name):
    # 使用 "vmss" 作为分隔符来切割字符串
    # print("node_name", node_name)
    parts = node_name.split("vmss")
    if len(parts) != 2:
        raise ValueError("Invalid node name format")
    base36_id = parts[1]

    try:
        # 将 36 进制的尾数转换为 10 进制
        numeric_id = int(base36_id, 36)
    except ValueError as e:
        logging.error(f"Failed to convert base36 to base10: {e}")
        raise ValueError(f"Failed to convert base36 to base10: {e}")
    # 构造 VMSS 实例 ID
    instance_id = str(numeric_id)
    # print("instance_id", instance_id)
    return instance_id

def get_kubernetes_client():
    kubeconfig_path = os.getenv('KUBECONFIG')
    if kubeconfig_path and os.path.isfile(kubeconfig_path):
        # 使用环境变量指定的kubeconfig文件
        config.load_kube_config(config_file=kubeconfig_path)
        logging.info(f"Using kubeconfig file at {kubeconfig_path}")
    else:
        # 尝试从默认位置加载kubeconfig
        home = os.path.expanduser('~')
        default_kubeconfig_path = os.path.join(home, '.kube', 'config')
        if os.path.isfile(default_kubeconfig_path):
            config.load_kube_config(config_file=default_kubeconfig_path)
            logging.info(f"Using kubeconfig file at {default_kubeconfig_path}")
        else:
            # 尝试加载集群内部配置
            try:
                config.load_incluster_config()
                logging.info("Using in-cluster kubeconfig")
            except config.ConfigException:
                raise FileNotFoundError("Could not find kubeconfig file and not running inside a cluster")

    # 创建API客户端
    clientset = client.CoreV1Api()
    return clientset

# Azure设置
subscription_id = os.environ['AZURE_SUBSCRIPTION_ID']
resource_group_name = os.environ['VMSS_RG']
vmss_name = os.environ['VMSS_NAME']
logging.info(f"Subscription ID: {subscription_id}, Resource Group: {resource_group_name}, VMSS Name: {vmss_name}")

# 获取默认Azure凭据
credential = DefaultAzureCredential()

# 创建ComputeManagementClient
compute_client = ComputeManagementClient(credential, subscription_id)
# 假设你已经有了AKS节点的数量
# config.load_kube_config()

# 创建API客户端
v1 = get_kubernetes_client()

# 要查找的节点标签
label_selector = 'agentpool='+ os.getenv('FOCUS_NODEPOOL')

polling_interval = int(os.getenv('CLEAN_INTERVAL', '60'))  # 默认值为60秒
previous_excess_vm_instance_ids = set()

while True:
    try:
        # 获取所有节点
        nodes = v1.list_node(label_selector=label_selector)
        ready_nodes = []  
        # aks_node_count = 0
        for node in nodes.items:
            # 检查节点是否处于Ready状态
            for condition in node.status.conditions:
                if condition.type == 'Ready' and condition.status == 'True':
                    ready_nodes.append(node.metadata.name)
        logging.info(f"Number of Ready nodes with label '{label_selector}': {len(ready_nodes)}")

        aks_instance_ids = [convert_node_name_to_vmss(name) for name in ready_nodes]
        logging.info(f"AKS VMSS instance IDs: {aks_instance_ids}")
        # 获取VMSS的实例数量
        vmss_vms = compute_client.virtual_machine_scale_set_vms.list(resource_group_name, vmss_name)
        vm_instance_ids = []
        for vm_instance in vmss_vms:
            # 获取实例视图以检查电源状态
            instance_view = compute_client.virtual_machine_scale_set_vms.get_instance_view(
                resource_group_name, vmss_name, vm_instance.instance_id
            )
            # 检查电源状态是否为'运行中'
            for status in instance_view.statuses:
                print(status.code)
                if status.code == 'ProvisioningState/succeeded':
                    vm_instance_ids.append(vm_instance.instance_id)
                    break
        logging.info(f"Current VMSS instance IDs: {vm_instance_ids}")
        current_excess_vm_instance_ids = set(vm_id for vm_id in vm_instance_ids if vm_id not in aks_instance_ids)
        logging.info(f"Current Candidate Delete VMSS instance IDs: {current_excess_vm_instance_ids}")
        to_delete_vm_instance_ids = current_excess_vm_instance_ids.intersection(previous_excess_vm_instance_ids)
        if to_delete_vm_instance_ids:
            logging.info(f"Deleting VM instances: {to_delete_vm_instance_ids}")
            # 调用API批量删除VM实例
            vm_instance_ids_model = VirtualMachineScaleSetVMInstanceRequiredIDs(instance_ids=list(to_delete_vm_instance_ids)
)
            async_delete_operation = compute_client.virtual_machine_scale_sets.begin_delete_instances(
                resource_group_name,
                vmss_name,
                # vm_instance_ids=list(to_delete_vm_instance_ids),
                vm_instance_i_ds=vm_instance_ids_model,
            )
            # 等待删除操作完成
            async_delete_operation.wait()
            logging.info(f"Deleted VM instances: {to_delete_vm_instance_ids}")

        # 更新上一轮的记录为这一轮的结果
        previous_excess_vm_instance_ids = current_excess_vm_instance_ids
        # logging.info(f"Current VMSS instance IDs: {previous_excess_vm_instance_ids}")
    except ApiException as e:
        logging.error(f"Exception when calling CoreV1Api->list_node: {e}")
        os._exit(1)
    except Exception as e:
        logging.error(f"An error occurred: {e}")
        os._exit(1)
    time.sleep(polling_interval)