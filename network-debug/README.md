# Kubernetes Network Debugging Toolkit

This toolkit provides a comprehensive solution for debugging network issues in Kubernetes clusters, specifically designed for investigating packet losses in overlay networks.

## Features

- **Automated Interface Detection**: Automatically detects bridge and overlay network interfaces on each node
- **Distributed Packet Capture**: Runs tcpdump on all nodes in the cluster as a DaemonSet
- **Web-based Analysis UI**: Provides a simple web interface to browse, download, and analyze capture files
- **Capture Rotation**: Automatically rotates capture files to prevent disk space issues

## Prerequisites

- Kubernetes cluster with kubectl access
- Sufficient permissions to create resources in the kube-system namespace

## Deployment

The toolkit includes a deployment script that makes it easy to deploy and manage:

```bash
# Deploy the toolkit
./deploy.sh deploy

# Check the status
./deploy.sh status

# Access the analyzer UI
./deploy.sh port-forward
# or
kubectl -n kube-system port-forward $(kubectl -n kube-system get pods -l app=tcpdump-analyzer -o name) 8080:8080
```

Then access the analyzer UI at http://localhost:8080

## Components

1. **tcpdump DaemonSet**: Runs on all nodes and captures packets on bridge interfaces
2. **Analyzer Deployment**: Provides a web UI for browsing and analyzing capture files
3. **ConfigMap**: Contains scripts for packet capture and analysis
4. **PersistentVolumeClaim**: Stores capture files

## Debugging Packet Loss in Overlay Networks

This toolkit is particularly useful for debugging packet loss in overlay networks. Here's how to use it effectively:

1. **Deploy the toolkit** to start capturing packets on all nodes
2. **Generate traffic** that reproduces the packet loss issue
3. **Access the analyzer UI** to examine the capture files
4. **Compare captures** from different nodes to identify where packets are being dropped
5. **Look for patterns** in the packet loss, such as specific protocols, packet sizes, or source/destination pairs

Common causes of packet loss in overlay networks:

1. **MTU misconfigurations**: Check if packets larger than the MTU are being dropped
2. **Network congestion**: Look for retransmissions and out-of-order packets
3. **Firewall rules**: Check if specific traffic is being blocked
4. **CNI plugin issues**: Compare behavior with different CNI configurations
5. **Physical network problems**: Look for patterns that might indicate hardware issues

## Cleanup

To remove all deployed resources:

```bash
./deploy.sh cleanup
```

## Customization

You can customize the toolkit by modifying the following files:

- **tcpdump-daemonset.yaml**: Modify the DaemonSet configuration
- **tcpdump-configmap.yaml**: Customize the capture and analysis scripts
- **capture-analyzer.yaml**: Adjust the analyzer deployment and service

## Troubleshooting

If you encounter issues:

1. Check the logs of the tcpdump pods:
   ```bash
   kubectl -n kube-system logs -l app=tcpdump-bridge
   ```

2. Check the logs of the analyzer pod:
   ```bash
   kubectl -n kube-system logs -l app=tcpdump-analyzer
   ```

3. Ensure the PersistentVolumeClaim is bound:
   ```bash
   kubectl -n kube-system get pvc