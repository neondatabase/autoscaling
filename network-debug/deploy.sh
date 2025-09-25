#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Function to display usage
usage() {
  echo "Network Debug Toolkit - tcpdump for Kubernetes"
  echo ""
  echo "Usage: $0 [command]"
  echo ""
  echo "Commands:"
  echo "  deploy      Deploy the tcpdump DaemonSet and analyzer"
  echo "  status      Check the status of the deployment"
  echo "  logs        Show logs from tcpdump pods"
  echo "  port-forward Start port forwarding to access the analyzer UI"
  echo "  cleanup     Remove all deployed resources"
  echo ""
}

# Deploy the tcpdump DaemonSet and analyzer
deploy() {
  echo "Deploying tcpdump DaemonSet and analyzer..."
  kubectl apply -k .
  
  echo ""
  echo "Waiting for pods to be ready..."
  kubectl -n kube-system wait --for=condition=Ready pods -l app=tcpdump-bridge --timeout=60s || true
  kubectl -n kube-system wait --for=condition=Ready pods -l app=tcpdump-analyzer --timeout=60s || true
  
  echo ""
  echo "Deployment complete. Use '$0 status' to check status."
  echo "Use '$0 port-forward' to access the analyzer UI."
}

# Check the status of the deployment
status() {
  echo "DaemonSet status:"
  kubectl -n kube-system get daemonset tcpdump-bridge
  
  echo ""
  echo "tcpdump pods:"
  kubectl -n kube-system get pods -l app=tcpdump-bridge -o wide
  
  echo ""
  echo "Analyzer daemonset:"
  kubectl -n kube-system get daemonset tcpdump-analyzer
  
  echo ""
  echo "Analyzer pod:"
  kubectl -n kube-system get pods -l app=tcpdump-analyzer
}

# Show logs from tcpdump pods
logs() {
  POD_NAME=$1
  
  if [ -z "$POD_NAME" ]; then
    echo "Available tcpdump pods:"
    kubectl -n kube-system get pods -l app=tcpdump-bridge -o name
    echo ""
    echo "Usage: $0 logs [pod-name]"
    echo "Example: $0 logs pod/tcpdump-bridge-abc123"
    return 1
  fi
  
  kubectl -n kube-system logs "$POD_NAME"
}

# Start port forwarding to access the analyzer UI
port_forward() {
  # Get the first available analyzer pod
  ANALYZER_POD=$(kubectl -n kube-system get pods -l app=tcpdump-analyzer -o name | head -n 1)
  
  if [ -z "$ANALYZER_POD" ]; then
    echo "Analyzer pod not found. Make sure the daemonset is running."
    return 1
  fi
  
  echo "Starting port forwarding to analyzer UI..."
  echo "Access the UI at: http://localhost:8080"
  echo "The UI will show capture files from all nodes in the cluster."
  echo "Press Ctrl+C to stop"
  
  kubectl -n kube-system port-forward "$ANALYZER_POD" 8080:8080
}

# Remove all deployed resources
cleanup() {
  echo "Removing all deployed resources..."
  kubectl delete -k . || true
  
  echo ""
  echo "Cleanup complete."
}

# Main script execution
if [ $# -eq 0 ]; then
  usage
  exit 1
fi

case "$1" in
  deploy)
    deploy
    ;;
  status)
    status
    ;;
  logs)
    logs "$2"
    ;;
  port-forward)
    port_forward
    ;;
  cleanup)
    cleanup
    ;;
  *)
    usage
    exit 1
    ;;
esac