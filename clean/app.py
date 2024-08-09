from datetime import datetime
import os
import logging
import time
import requests

from azure.identity import DefaultAzureCredential
from azure.mgmt.compute import ComputeManagementClient
from kubernetes import client, config
from kubernetes.client.rest import ApiException
from azure.mgmt.compute.models import VirtualMachineScaleSetVMInstanceRequiredIDs


logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s %(levelname)s: %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
azure_logger = logging.getLogger('azure')
azure_logger.setLevel(logging.WARNING)


def send_feishu_webhook(clusterName, nodepool, vmss_name, vmComputeName, vm_id):
    webhook_url = os.getenv('WEBHOOK_URL')
    if not webhook_url:
        logging.error("Webhook URL is not set in environment variables.")
        return
    # 发送告警消息的逻辑，例如使用 requests 库
    headers = {'Content-Type': 'application/json'}
    message = f'''AKS Cluster: {clusterName} \
        \n NodePool: {nodepool}
        \n Azure VMSS: {vmss_name} \
        \n 删除失败的实例: {vmComputeName} \
        \n 对应的实例 VMSS InstanceID：{vm_id}'''

    body = {"msg_type": "interactive",
            "card": {"header": {"title": {"tag": "plain_text", "content": "Azure AKS VMSS 实例删除失败告警"},
                                "template": "orange"},
                     "elements": [{"tag": "markdown", "content": message}]}}
    requests.post(url=webhook_url, headers=headers, json=body)
    logging.info(f"Alert sent to Webhook URL: {webhook_url}")


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
clusterName = os.getenv('AKS_NAME')
label_selector = 'agentpool=' + os.getenv('FOCUS_NODEPOOL')

polling_interval = int(os.getenv('CLEAN_INTERVAL', '60'))  # 默认值为60秒
previous_excess_vm_instance_ids = set()

# 从环境变量中获取通知间隔时间（秒）
notification_interval = int(os.getenv('NOTIFICATION_INTERVAL', '300'))  # 默认300秒（5分钟）

# 保存应该删除的VM实例及其首次标记删除的时间
pending_deletion_vms = {}

while True:
    try:
        # 获取所有节点
        logging.info(f"Start to check nodes:\n======================")
        nodes = v1.list_node(label_selector=label_selector)
    except Exception as e:
        logging.error(f"An error occurred while listing nodes: {e}")
        os._exit(1)

    try:
        ready_nodes = []
        for node in nodes.items:
            ready_nodes.append(node.metadata.name)
        logging.info(f"Number of nodes with label '{label_selector}': {len(ready_nodes)}")
        logging.info(f"AKS nodes name: {ready_nodes}")
        aks_instance_ids = [convert_node_name_to_vmss(name) for name in ready_nodes]
        logging.info(f"AKS VMSS instance IDs: {aks_instance_ids}")

        # 获取VMSS的实例数量
        vmss_vms = compute_client.virtual_machine_scale_set_vms.list(resource_group_name, vmss_name)
        vm_instance_ids, vm_instance_computer_names = [], []
        for vm_instance in vmss_vms:
            instance_view = compute_client.virtual_machine_scale_set_vms.get_instance_view(
                resource_group_name, vmss_name, vm_instance.instance_id
            )
            for status in instance_view.statuses:
                if status.code == 'ProvisioningState/succeeded':
                    vm_instance_ids.append(vm_instance.instance_id)
                    vm_instance_computer_names.append(vm_instance.os_profile.computer_name)
                    break
        logging.info(f"Current VMSS instance Computer Names: {vm_instance_computer_names}")
        logging.info(f"Current VMSS instance IDs: {vm_instance_ids}")
        vm_id_to_name = {vm_id: name for vm_id, name in zip(vm_instance_ids, vm_instance_computer_names)}

        # 找出需要删除的多余VM实例
        current_excess_vm_instance_ids = set(vm_id for vm_id in vm_instance_ids if vm_id not in aks_instance_ids)
        # logging.info(f"Current Candidate Delete VMSS instance Computer Names: {current_excess_vm_computer_names}")
        logging.info(f"Current Candidate Delete VMSS instance IDs: {current_excess_vm_instance_ids}")

        to_delete_vm_instance_ids = current_excess_vm_instance_ids.intersection(previous_excess_vm_instance_ids)
        to_delete_vm_instance_ids = to_delete_vm_instance_ids - set(pending_deletion_vms.keys())

        if to_delete_vm_instance_ids:
            logging.info(f"Deleting VM instances: {to_delete_vm_instance_ids}")
            vm_instance_ids_model = VirtualMachineScaleSetVMInstanceRequiredIDs(
                instance_ids=list(to_delete_vm_instance_ids))
            async_delete_operation = compute_client.virtual_machine_scale_sets.begin_delete_instances(
                resource_group_name,
                vmss_name,
                vm_instance_ids_model,
            )

            # 将要删除的VM实例及其首次标记删除时间保存到pending_deletion_vms
            now = datetime.utcnow()
            for vm_id in to_delete_vm_instance_ids:
                if vm_id not in pending_deletion_vms:
                    pending_deletion_vms[vm_id] = now
                    logging.info(f"Recorded deletion time for VM instance {vm_id}: {now}")

        # 检查之前待删除的VM实例是否已经删除
        current_time = datetime.utcnow()
        for vm_id, deletion_time in list(pending_deletion_vms.items()):
            if vm_id in vm_instance_ids:
                elapsed_time = current_time - deletion_time
                if elapsed_time.total_seconds() > notification_interval:
                    logging.error(
                        f"VM instance {vm_id} was not deleted within the expected time of {notification_interval} seconds.")
                    # send_feishu_webhook(clusterName, os.getenv('FOCUS_NODEPOOL'), vmss_name, vm_id_to_name[vm_id], vm_id)
                    # 更新记录的删除时间为当前时间
                    pending_deletion_vms[vm_id] = current_time
            else:
                # 如果实例不再存在，说明删除成功，从待删除列表中移除
                logging.info(f"VM instance {vm_id} has been successfully deleted.")
                del pending_deletion_vms[vm_id]

        previous_excess_vm_instance_ids = current_excess_vm_instance_ids
    except ApiException as e:
        logging.error(f"Exception when calling CoreV1Api->list_node: {e}")
        os._exit(1)
    except Exception as e:
        logging.error(f"An error occurred: {e}")
        os._exit(1)

    time.sleep(polling_interval)
