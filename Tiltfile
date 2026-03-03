allow_k8s_contexts(['kind-mgmt', 'kind-worker'])

mgmt_ctx = 'kind-mgmt'
worker_ctx = 'kind-worker'

mgmt_kubeconfig = './config/kind/mgmt-kubeconfig-external'
worker_kubeconfig = './config/kind/worker-kubeconfig'

def mgmt_kubectl(args):
    return local('kubectl --kubeconfig=' + mgmt_kubeconfig + ' --context=' + mgmt_ctx + ' ' + args)

def worker_kubectl(args):
    return local('kubectl --kubeconfig=' + worker_kubeconfig + ' --context=' + worker_ctx + ' ' + args)

METAL_OPERATOR_REF = "v0.4.0"
mgmt_kubectl('apply -f https://raw.githubusercontent.com/ironcore-dev/metal-operator/' + METAL_OPERATOR_REF + '/config/crd/bases/metal.ironcore.dev_serverclaims.yaml')
mgmt_kubectl('apply -f https://raw.githubusercontent.com/ironcore-dev/metal-operator/' + METAL_OPERATOR_REF + '/config/crd/bases/metal.ironcore.dev_servermaintenances.yaml')
mgmt_kubectl('apply -f https://raw.githubusercontent.com/ironcore-dev/metal-operator/' + METAL_OPERATOR_REF + '/config/crd/bases/metal.ironcore.dev_servers.yaml')
mgmt_kubectl('wait --for=condition=Established --timeout=60s crd/serverclaims.metal.ironcore.dev')
mgmt_kubectl('wait --for=condition=Established --timeout=60s crd/servermaintenances.metal.ironcore.dev')
mgmt_kubectl('wait --for=condition=Established --timeout=60s crd/servers.metal.ironcore.dev')
mgmt_kubectl('apply -f config/kind/crs/server.yaml')
mgmt_kubectl('apply -f config/kind/crs/serverclaim.yaml')

worker_kubectl('apply -k config/kind') # kustomize
worker_kubectl('apply -f config/kind/crs/node.yaml')

local_resource(
    "manager-binary",
    cmd = 'mkdir -p .tiltbuild; CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .tiltbuild/manager ./cmd/metal-cloud-controller-manager/main.go',
    deps = ["pkg", "cmd", "go.mod", "go.sum"]
)

docker_build(
    ref = "controller",
    context = "./.tiltbuild/",
    dockerfile_contents = """
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY manager /metal-cloud-controller-manager
USER 65532:65532
ENTRYPOINT ["/metal-cloud-controller-manager"]
""",
    only = "manager"
)

k8s_yaml(kustomize('config/kind'))

k8s_resource(
    'cloud-controller-manager',
    labels=['CCM'],
    port_forwards='10258:10258',
    extra_pod_selectors=[{'app.kubernetes.io/name': 'cloud-controller-manager'}]
)