import itertools
import json
import os
import sys

source_tag = os.getenv("SOURCE_TAG")
target_tag = os.getenv("TARGET_TAG")
dev_acr = os.getenv("DEV_ACR")
prod_acr = os.getenv("PROD_ACR")
dev_aws = os.getenv("DEV_AWS")
prod_aws = os.getenv("PROD_AWS")
aws_region = os.getenv("AWS_REGION")

components = json.loads(os.getenv("COMPONENTS"))
assert isinstance(components, list), "COMPONENTS must be a JSON array"

registries = {
    "dev": [
        "docker.io/neondatabase",
        "ghcr.io/neondatabase",
        f"{dev_aws}.dkr.ecr.{aws_region}.amazonaws.com",
        f"{dev_acr}.azurecr.io/neondatabase",
    ],
    "prod": [
        f"{prod_aws}.dkr.ecr.{aws_region}.amazonaws.com",
        f"{prod_acr}.azurecr.io/neondatabase",
    ],
}

# Map of environment -> image map (map of source image -> list of target images to push to)
outputs: dict[str, dict[str, list[str]]] = {}

for stage in ("dev", "prod"):
    outputs[stage] = {
        f"ghcr.io/neondatabase/{component}:{source_tag}": [
            f"{registry}/{component}:{tag}"
            for registry, tag in itertools.product(registries[stage], [target_tag])
            if not (registry == "ghcr.io/neondatabase" and tag == source_tag)
        ]
        for component in components
    }

with open(os.getenv("GITHUB_OUTPUT", "/dev/null"), "a") as f:
    for key, value in outputs.items():
        f.write(f"{key}={json.dumps(value)}\n")
        print(f"Image map for {key}:\n{json.dumps(value, indent=2)}\n\n", file=sys.stderr)
