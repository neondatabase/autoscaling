#!/bin/bash

folder=`cd $(dirname "${BASH_SOURCE[@]}"); pwd`

set -a
[ -f ${folder}/settings ] && source ${folder}/settings
[ -f ${folder}/settings.local ] && source ${folder}/settings.local
set +a

bold=$(tput bold)
normal=$(tput sgr0)
red=$(tput setaf 1)
green=$(tput setaf 2)

_die() {
    echo "${red}ERROR:${normal} something going wrong, exiting..."
    exit 1
}

# check to see if this file is being run or sourced from another script
_is_sourced() {
    # https://unix.stackexchange.com/a/215279
    [ "${#FUNCNAME[@]}" -ge 2 ] \
        && [ "${FUNCNAME[0]}" = '_is_sourced' ] \
        && [ "${FUNCNAME[1]}" = 'source' ]
}

# check utils used in script present
_check_utils() {
    utils=("$@")
    for util in "${utils[@]}"; do
        [ -z "$(which ${util})" ] && echo "${bold}${red}${util}${normal} not found" && util_not_found=true
    done
    [ -n "${util_not_found}" ] && exit 1
}

# check vaiables are set (not empty)
_check_vars()
{
    var_names=("$@")
    for var_name in "${var_names[@]}"; do
        [ -z "${!var_name}" ] && echo "${bold}${red}${var_name}${normal} is unset, check your settings and/or settings.local files" && var_unset=true
    done
    [ -n "$var_unset" ] && exit 1
}

_usage() {
    echo "Usage: $(basename $0) <command>"
    cat <<-'EOT'

Commands:

    help	display this help and exit

    create	create cluster
    destroy	destroy cluster
    status	check current state

EOT
}

_get_vpc_data() {
    echo "${bold}Retrieving data from AWS${normal}"
    vpcid=$(aws ec2 describe-vpcs --filters Name=tag-value,Values=${AWS_NAME} | jq -r '.Vpcs[].VpcId')
    if [ "${vpcid}" ]; then
        subnetid=$(aws ec2 describe-subnets --filters Name=tag-value,Values=${AWS_NAME} Name=vpc-id,Values=${vpcid} | jq -r '.Subnets[].SubnetId')
        groupid=$(aws ec2 describe-security-groups --filters Name=tag-value,Values=${AWS_NAME} Name=vpc-id,Values=${vpcid} | jq -r '.SecurityGroups[].GroupId')
        rtid=$(aws ec2 describe-route-tables --filters Name=tag-value,Values=${AWS_NAME} Name=vpc-id,Values=${vpcid} | jq -r '.RouteTables[].RouteTableId')
        gatewayid=$(aws ec2 describe-internet-gateways --filters Name=tag-value,Values=${AWS_NAME} Name=attachment.vpc-id,Values=${vpcid} | jq -r '.InternetGateways[].InternetGatewayId')
    fi
    keypair=`aws ec2 describe-key-pairs | jq -r ".KeyPairs[]|select(.KeyName == \"${AWS_NAME}\")|.KeyName"`
}

_create_vpc() {
    echo "${bold}Adding VPC resources${normal}"
    if [ -z $vpcid ]; then
        echo '   create VPC'; vpcid=$(aws ec2 create-vpc --cidr-block ${AWS_VPC_CIDR} | jq -r '.Vpc.VpcId')
        echo '   tag VPC'; aws ec2 create-tags --resources ${vpcid} --tags Key=Name,Value=${AWS_NAME}
    else
        echo '   VPC created already'
    fi
    if [ -z $subnetid ]; then
        echo '   create subnet'; subnetid=$(aws ec2 create-subnet --vpc-id ${vpcid}  --cidr-block ${AWS_SUBNET_CIDR} | jq -r '.Subnet.SubnetId')
        echo '   set subnet as public'; aws ec2 modify-subnet-attribute --subnet-id ${subnetid} --map-public-ip-on-launch
        echo '   tag subnet'; aws ec2 create-tags --resources ${subnetid} --tags Key=Name,Value=${AWS_NAME}
    else
        echo '   subnet created already'
    fi
    if [ -z $gatewayid ]; then
        echo '   create  internet gateway'; gatewayid=$(aws ec2 create-internet-gateway | jq -r '.InternetGateway.InternetGatewayId')
        echo '   tag gateway'; aws ec2 create-tags --resources ${gatewayid} --tags Key=Name,Value=${AWS_NAME}
        echo '   attach gateway to VPC'; aws ec2 attach-internet-gateway --vpc-id ${vpcid} --internet-gateway-id ${gatewayid}
    else
        echo '   internet gateway created already'
    fi
    if [ -z $rtid ]; then
        echo '   obtain default route table'; rtid=`aws ec2 describe-route-tables --filters Name=vpc-id,Values=${vpcid} | jq -r '.RouteTables[]|select(.Associations[].Main == true)|.RouteTableId'`
        echo '   tag route table'; aws ec2 create-tags --resources ${rtid} --tags Key=Name,Value=${AWS_NAME}
        echo '   add default route to gateway'; aws ec2 create-route --route-table-id ${rtid} --destination-cidr-block 0.0.0.0/0 --gateway-id ${gatewayid} >/dev/null
        echo '   attach subnet to route table'; aws ec2 associate-route-table --subnet-id ${subnetid} --route-table-id ${rtid} >/dev/null
    else
        echo '   route table configured already'
    fi
    if [ -z $groupid ]; then
        echo '   create security group'; groupid=$(aws ec2 create-security-group --group-name ${AWS_NAME} --description ${AWS_NAME} --vpc-id ${vpcid} | jq -r '.GroupId')
        echo '   tag group'; aws ec2 create-tags --resources ${groupid} --tags Key=Name,Value=${AWS_NAME}
        echo '   add open rule to group'; aws ec2 authorize-security-group-ingress --group-id ${groupid} --ip-permissions '[{"IpProtocol": "-1", "IpRanges": [{"CidrIp": "0.0.0.0/0"}]}]' >/dev/null
    else
        echo '   security group created already'
    fi
    if [ "${keypair}" != "${AWS_NAME}" ]; then
        echo '   create ssh keypair'
        [ -f ${AWS_NAME}.pem ] && rm -f ${AWS_NAME}.pem
        aws ec2 create-key-pair --key-name ${AWS_NAME} --query 'KeyMaterial' --output text > ${AWS_NAME}.pem
        chmod 0400 ${AWS_NAME}.pem
    else
        echo '   ssh keypair created already'
    fi
    echo 'done'
}

_del_vpc() {
    echo "${bold}Removing VPC resources${normal}"
    if [ "${groupid}" ]; then
        echo '   delete security group'; aws ec2 delete-security-group --group-id ${groupid} || die
    else
        echo '   security group not found, nothing to delete'
    fi
    if [ "${subnetid}" ]; then
        echo '   delete subnet'; aws ec2 delete-subnet --subnet-id ${subnetid} || die
    else
        echo '   subnet not found, nothing to delete'
    fi
    if [ "${gatewayid}" -a "${vpcid}" ]; then
        echo '   detach internet gateway from VPC'; aws ec2 detach-internet-gateway --internet-gateway-id ${gatewayid} --vpc-id ${vpcid} || die
    else
        echo '   internet gateway not attached to vpc'
    fi
    if [ "${gatewayid}" ]; then
        echo '   delete internet gateway'; aws ec2 delete-internet-gateway --internet-gateway-id ${gatewayid} || die
    else
        echo '   internet gateway not found, nothing to delete'
    fi
    if [ "${vpcid}" ]; then
        echo '   delete VPC'; aws ec2 delete-vpc --vpc-id ${vpcid} || die
    else
        echo '   VPC not found, nothing to delete'
    fi
    if [ -f "${AWS_NAME}".pem ]; then
        echo '   delete ssh keypair'; rm -f "${AWS_NAME}".pem; aws ec2 delete-key-pair --key-name ${AWS_NAME} || die
    else
        echo '   ssh ketpair not found, nothing to delete'
    fi
    echo "done"
}

_status() {
    if [ "$vpcid"  ]; then
        echo 'VPC has been created'
    else
        echo 'VPC not found'
    fi
    for i in $(seq 1 ${K3S_CLUSTER_SIZE}); do
        if [ "${nodeid[$i]}" ]; then
            echo "'${AWS_NAME}-${i}' running, try: $(tput setaf 2)ssh -i ${AWS_NAME}.pem ubuntu@${nodeippublic[$i]}$(tput sgr0)"
            #echo "Instance '${AWS_NAME}-${i}'(id: ${nodeid[$i]}) started, use $(tput setaf 2)ssh -i ${AWS_NAME}.pem ubuntu@${nodeippublic[$i]}$(tput sgr0)"
        else
            echo "Instance '${AWS_NAME}-${i}' not found"
        fi
    done
}

_get_ami() {
    ami=$(curl -sfSL https://cloud-images.ubuntu.com/query/jammy/server/released.current.txt \
        | grep ${AWS_REGION} | grep amd64 | grep hvm | grep ebs-ssd | awk '{print $8}')
    [ -z "${ami}" ] && die "AMI image not found"
}

_create_nodes() {
    for i in $(seq 1 ${K3S_CLUSTER_SIZE}); do

        echo "${bold}Starting '${AWS_NAME}-${i}' instance${normal}"

        if [ "${nodeid[$i]}" ]; then
            echo "   instance '${AWS_NAME}-${i}' started already"
            continue
        fi
        echo '   obtain AMI id for Ubuntu 22.04'; _get_ami
        echo '   run instance'
        node=$(aws ec2 run-instances \
            --image-id "${ami}" \
            --key-name "${AWS_NAME}" \
            --security-group-ids "${groupid}" \
            --instance-type "${EC2_TYPE}" \
            --block-device-mappings 'DeviceName=/dev/sda1,Ebs={VolumeSize=128,VolumeType=gp3}' \
            --subnet-id "${subnetid}")
        nodeid[${i}]=$(echo ${node} | jq -r '.Instances[].InstanceId')
        if [ -z "${nodeid[$i]}" ]; then
            echo '   instance not started!'
            die
        fi
        echo '   tag instance'; aws ec2 create-tags --resources ${nodeid[$i]} --tags Key=Name,Value="${AWS_NAME}-${i}"
        echo '   wait while instance initalized'
        echo -n '   '
        while [ "$(aws ec2 describe-instances --instance-ids ${nodeid[$i]} | jq -r '.Reservations[].Instances[].State.Name')" != "running"  ]; do
            echo -n '.'
            sleep 1
        done
        echo
        echo "   instance started (id: ${nodeid[$i]})"
        nodeippublic[${i}]=$(aws ec2 describe-instances  --instance-ids ${nodeid[$i]} | jq -r '.Reservations[].Instances[].PublicIpAddress')
        nodeipprivate[${i}]=$(aws ec2 describe-instances --instance-ids ${nodeid[$i]} | jq -r '.Reservations[].Instances[].PrivateIpAddress')
        echo 'done'
    done

    # Store private IP's of all nodes in comma separated string.
    # This uses bash pattern substitution: ${parameter//pattern/string} will
    # replace each instance of pattern in $parameter with string. In this case,
    # string is ${IFS:0:1}, the substring of $IFS starting at the beginning
    # and ending after one character.
    t="${nodeipprivate[@]}"
    privateips="${t//${IFS:0:1}/,}"
}

_wait_nodes() {
    for i in $(seq 1 ${K3S_CLUSTER_SIZE}); do

        echo "${bold}Waiting '${AWS_NAME}-${i}' instance become ready${normal}"
        echo '   check ssh connection'
        echo -n '   '
        while ! ssh -i ${AWS_NAME}.pem  -q -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=5 ubuntu@${nodeippublic[$i]} uname >/dev/null 2>&1; do
            echo -n '.'; sleep 1
        done
        echo
        echo '   connected'
        echo 'done'
    done
}

_del_nodes() {
    for i in $(seq ${K3S_CLUSTER_SIZE} 1); do
        echo "${bold}Deleting '${AWS_NAME}-${i}' instance${normal}"
        if [ -z "${nodeid[$i]}" ]; then
            echo "   instance '${AWS_NAME}-${i}' not found (may be deleted already)"
            continue
        fi
        if [ "$(aws ec2 describe-instances --instance-ids ${nodeid[$i]} | jq -r '.Reservations[].Instances[].State.Name' | uniq)" ]; then
            aws ec2 terminate-instances --instance-ids ${nodeid[$i]} >/dev/null 2>&1
        fi
    done

    for i in $(seq ${K3S_CLUSTER_SIZE} 1); do
        if [ -z "${nodeid[$i]}" ]; then
            continue
        fi
        if [ "$(aws ec2 describe-instances --instance-ids ${nodeid[$i]} | jq -r '.Reservations[].Instances[].State.Name' | uniq)" ]; then
            echo "${bold}Waiting for '${AWS_NAME}-${i}' termination${normal}"
            echo -n '   '
            while [ "$(aws ec2 describe-instances --instance-ids ${nodeid[$i]} | jq -r '.Reservations[].Instances[].State.Name' | uniq)" != "terminated" ]; do
                echo -n '.'; sleep 3
            done
            echo
            echo "   instance terminated (id: ${nodeid[$i]})"
        fi
    done
}

_get_nodes() {
    echo "${bold}Retrieving instances details${normal}"
    for i in $(seq 1 ${K3S_CLUSTER_SIZE}); do
        nodeid[${i}]=$(aws ec2 describe-instances        --filters Name=tag-value,Values=${AWS_NAME}-${i} Name=vpc-id,Values=${vpcid} | jq -r '.Reservations[].Instances[].InstanceId')
        nodeippublic[${i}]=$(aws ec2 describe-instances  --filters Name=tag-value,Values=${AWS_NAME}-${i} Name=vpc-id,Values=${vpcid} | jq -r '.Reservations[].Instances[].PublicIpAddress')
        nodeipprivate[${i}]=$(aws ec2 describe-instances --filters Name=tag-value,Values=${AWS_NAME}-${i} Name=vpc-id,Values=${vpcid} | jq -r '.Reservations[].Instances[].PrivateIpAddress')
    done

    # Store private IP's of all nodes in comma separated string.
    # This uses bash pattern substitution: ${parameter//pattern/string} will
    # replace each instance of pattern in $parameter with string. In this case,
    # string is ${IFS:0:1}, the substring of $IFS starting at the beginning
    # and ending after one character.
    t="${nodeipprivate[@]}"
    privateips="${t//${IFS:0:1}/,}"
}

_provision() {
    for i in $(seq 1 ${K3S_CLUSTER_SIZE}); do

        echo "${bold}Provisioning '${AWS_NAME}-${i}' instance${normal}"

        if [ ${i} -eq 1 ]; then
            # provisioning first node - k3s master
            scriptname=${K3S_MASTER_SCRIPT}
        else
            # provisioning remain nodes - k3s workers
            scriptname=${K3S_WORKER_SCRIPT}
        fi
        scp -i ${AWS_NAME}.pem -q -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=5 ${scriptname} ubuntu@${nodeippublic[$i]}:/tmp/provision.sh
        ssh -t -i ${AWS_NAME}.pem -q -o StrictHostKeyChecking=no ubuntu@${nodeippublic[$i]} -- bash /tmp/provision.sh "${AWS_NAME}-${i}" "${K3S_TOKEN}" "${nodeipprivate[1]}" "${privateips}"
    done
}

_kubectlset() {
    if [ -n "${nodeid[1]}" ]; then
        echo "${bold}Setup k8s context '${AWS_NAME}' for local kubectl${normal}"
        rawconfig="$(ssh -q -o StrictHostKeyChecking=no -i ${AWS_NAME}.pem ubuntu@${nodeippublic[1]} -- sudo kubectl config view -ojson --raw)"

        cacert_file=$(mktemp)
        cert_file=$(mktemp)
        key_file=$(mktemp)

        echo "$rawconfig" | jq -r '.clusters[0].cluster["certificate-authority-data"]' | base64 --decode >$cacert_file
        echo "$rawconfig" | jq -r '.users[0].user["client-certificate-data"]' | base64 --decode >$cert_file
        echo "$rawconfig" | jq -r '.users[0].user["client-key-data"]' | base64 --decode >$key_file

        # create cluster entry in local kubectl config
        kubectl config set-cluster ${AWS_NAME} \
            --server=https://${nodeippublic[1]}:6443 \
            --certificate-authority=$cacert_file \
            --embed-certs=true
        kubectl config set-credentials ${AWS_NAME} \
            --client-certificate=$cert_file \
            --client-key=$key_file \
            --embed-certs=true
        rm -f $cacert_file $cert_file $key_file

        kubectl config set-context ${AWS_NAME} \
            --cluster=${AWS_NAME} \
            --user=${AWS_NAME}
        kubectl config use-context ${AWS_NAME}
    fi
}

_kubectldel() {
    echo "${bold}Delete k8s context '${AWS_NAME}'${normal}"
    kubectl config unset current-context
    kubectl config unset clusters.${AWS_NAME}
    kubectl config unset users.${AWS_NAME}
    kubectl config unset contexts.${AWS_NAME}
}

_initiate() {
    _check_vars AWS_PROFILE AWS_REGION EC2_TYPE AWS_VPC_CIDR AWS_SUBNET_CIDR AWS_NAME K3S_CLUSTER_SIZE K3S_TOKEN K3S_MASTER_SCRIPT K3S_WORKER_SCRIPT
    _check_utils aws jq curl ssh grep awk mktemp base64 kubectl
    if [ "${AWS_NAME}" = "change-me" ]; then
        echo "Cluster name '${AWS_NAME}'"
        echo "Seems you haven't changed AWS_NAME variable in settings.local file"
        echo
        exit
    fi
    _get_vpc_data
    [ "${vpcid}" ] && _get_nodes
}


_main() {
    echo
    echo -e "${bold}=== Dev AWS infra and K3S cluster ===${normal}"
    echo
    case "$1" in
        help)
            _usage
            ;;
        create)
            _initiate
            _create_vpc
            _create_nodes
            _wait_nodes
            _provision
            _kubectlset
            ;;
        destroy)
            _initiate
            _del_nodes
            _kubectldel
            _del_vpc
            ;;
        status)
            _initiate
            _status
            ;;
        *)
            _usage
            ;;
    esac
}

if ! _is_sourced; then
    _main "$@"
fi
