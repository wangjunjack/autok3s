package tencent

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cnrancher/autok3s/pkg/cluster"
	"github.com/cnrancher/autok3s/pkg/common"
	"github.com/cnrancher/autok3s/pkg/providers"
	"github.com/cnrancher/autok3s/pkg/types"
	"github.com/cnrancher/autok3s/pkg/types/tencent"
	"github.com/cnrancher/autok3s/pkg/utils"
	"github.com/cnrancher/autok3s/pkg/viper"

	"github.com/sirupsen/logrus"
	tencentCommon "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"
	tag "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tag/v20180813"
	tke "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/tke/v20180525"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
	"golang.org/x/sync/syncmap"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	k3sVersion              = ""
	k3sChannel              = "v1.18"
	secretID                = "secret-id"
	secretKey               = "secret-key"
	imageID                 = "img-pi0ii46r" /* Ubuntu Server 18.04.1 LTS x64 */
	instanceType            = "SA1.MEDIUM4"  /* CPU:2 Memory:4 */
	instanceChargeType      = "POSTPAID_BY_HOUR"
	internetMaxBandwidthOut = "50"
	internetChargeType      = "TRAFFIC_POSTPAID_BY_HOUR"
	diskCategory            = "CLOUD_SSD"
	diskSize                = "60"
	maxPageSize             = 100
	master                  = "0"
	worker                  = "0"
	ui                      = false
	repo                    = "https://apphub.aliyuncs.com"
	defaultCidr             = "10.42.0.0/16"
)

// ProviderName is the name of this provider.
const ProviderName = "tencent"

var (
	k3sScript        = "http://rancher-mirror.cnrancher.com/k3s/k3s-install.sh"
	k3sMirror        = "INSTALL_K3S_MIRROR=cn"
	dockerMirror     = ""
	deployCCMCommand = "echo \"%s\" | base64 -d | sudo tee \"%s/cloud-controller-manager.yaml\""
)

type checkFun func() error

type Tencent struct {
	types.Metadata  `json:",inline"`
	tencent.Options `json:",inline"`
	types.Status    `json:"status"`

	c      *cvm.Client
	v      *vpc.Client
	t      *tag.Client
	r      *tke.Client
	m      *sync.Map
	logger *logrus.Logger
}

func init() {
	providers.RegisterProvider(ProviderName, func() (providers.Provider, error) {
		return NewProvider(), nil
	})
}

func NewProvider() *Tencent {
	return &Tencent{
		Metadata: types.Metadata{
			Provider:   ProviderName,
			Master:     master,
			Worker:     worker,
			UI:         ui,
			Repo:       repo,
			K3sVersion: k3sVersion,
			K3sChannel: k3sChannel,
		},
		Options: tencent.Options{
			ImageID:                 imageID,
			InstanceType:            instanceType,
			SystemDiskSize:          diskSize,
			SystemDiskType:          diskCategory,
			InternetMaxBandwidthOut: internetMaxBandwidthOut,
			PublicIPAssignedEIP:     false,
		},
		Status: types.Status{
			MasterNodes: make([]types.Node, 0),
			WorkerNodes: make([]types.Node, 0),
		},
		m: new(syncmap.Map),
	}
}

func (p *Tencent) GetProviderName() string {
	return ProviderName
}

func (p *Tencent) GenerateClusterName() {
	p.Name = fmt.Sprintf("%s.%s", p.Name, p.Region)
}

func (p *Tencent) CreateK3sCluster(ssh *types.SSH) (err error) {
	p.logger = common.NewLogger(common.Debug)
	p.logger.Infof("[%s] executing create logic...\n", p.GetProviderName())

	defer func() {
		if err == nil && len(p.Status.MasterNodes) > 0 {
			fmt.Printf(common.UsageInfo, p.Name)
			if p.UI {
				if p.CloudControllerManager {
					fmt.Printf("\nK3s UI URL: https://<using `kubectl get svc -A` get UI address>:8999\n")
				} else {
					fmt.Printf("\nK3s UI URL: https://%s:8999\n", p.Status.MasterNodes[0].PublicIPAddress[0])
				}
			}
			fmt.Println("")
		}
	}()

	c, err := p.generateInstance(p.createCheck, ssh)
	if err != nil {
		return
	}

	// initialize K3s cluster.
	if err = cluster.InitK3sCluster(c); err != nil {
		return
	}
	p.logger.Infof("[%s] successfully executed create logic\n", p.GetProviderName())

	if option, ok := c.Options.(tencent.Options); ok {
		extraManifests := make([]string, 0)
		if c.CloudControllerManager {
			// deploy additional Tencent cloud-controller-manager manifests.
			tencentCCM := &tencent.CloudControllerManager{
				Region:                base64.StdEncoding.EncodeToString([]byte(option.Region)),
				SecretKey:             base64.StdEncoding.EncodeToString([]byte(option.SecretKey)),
				SecretID:              base64.StdEncoding.EncodeToString([]byte(option.SecretID)),
				VpcID:                 base64.StdEncoding.EncodeToString([]byte(option.VpcID)),
				NetworkRouteTableName: base64.StdEncoding.EncodeToString([]byte(p.Name)),
			}
			var tmpl string
			if p.ClusterCIDR == "" {
				tmpl = fmt.Sprintf(tencentCCMTmpl, tencentCCM.Region, tencentCCM.SecretID, tencentCCM.SecretKey,
					tencentCCM.VpcID, tencentCCM.NetworkRouteTableName, defaultCidr)
			} else {
				tmpl = fmt.Sprintf(tencentCCMTmpl, tencentCCM.Region, tencentCCM.SecretID, tencentCCM.SecretKey,
					tencentCCM.VpcID, tencentCCM.NetworkRouteTableName, p.ClusterCIDR)
			}

			extraManifests = append(extraManifests, fmt.Sprintf(deployCCMCommand,
				base64.StdEncoding.EncodeToString([]byte(tmpl)), common.K3sManifestsDir))
		}
		p.logger.Infof("[%s] start deploy tencent additional manifests\n", p.GetProviderName())
		if err := cluster.DeployExtraManifest(c, extraManifests); err != nil {
			return err
		}
		p.logger.Infof("[%s] successfully deploy tencent additional manifests\n", p.GetProviderName())
	}

	return
}

func (p *Tencent) JoinK3sNode(ssh *types.SSH) error {
	p.logger = common.NewLogger(common.Debug)
	p.logger.Infof("[%s] executing join logic...\n", p.GetProviderName())

	merged, err := p.generateInstance(p.joinCheck, ssh)
	if err != nil {
		return err
	}

	added := &types.Cluster{
		Metadata: merged.Metadata,
		Options:  merged.Options,
		Status:   types.Status{},
	}

	p.m.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		// filter the number of nodes that are not generated by current command.
		if v.Current {
			if v.Master {
				added.Status.MasterNodes = append(added.Status.MasterNodes, v)
			} else {
				added.Status.WorkerNodes = append(added.Status.WorkerNodes, v)
			}
		}
		return true
	})

	// join K3s node.
	if err := cluster.JoinK3sNode(merged, added); err != nil {
		return err
	}

	p.logger.Infof("[%s] successfully executed join logic\n", p.GetProviderName())

	return nil
}

func (p *Tencent) Rollback() error {
	p.logger.Infof("[%s] executing rollback logic...\n", p.GetProviderName())

	ids := make([]string, 0)
	p.m.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		if v.RollBack {
			ids = append(ids, key.(string))
		}
		return true
	})

	p.logger.Debugf("[%s] instances %s will be rollback\n", p.GetProviderName(), ids)

	if len(ids) > 0 {
		if p.PublicIPAssignedEIP {
			eips, err := p.describeAddresses(nil, tencentCommon.StringPtrs(ids))
			if err != nil {
				p.logger.Errorf("[%s] error when query eip info\n", p.GetProviderName())
			}
			var (
				eipIds  []string
				taskIds []uint64
			)
			for _, eip := range eips {
				eipIds = append(eipIds, *eip.AddressId)
				if taskID, err := p.disassociateAddress(*eip.AddressId); err != nil {
					p.logger.Warnf("[%s] disassociate eip [%s] error\n", p.GetProviderName(), *eip.AddressId)
				} else {
					taskIds = append(taskIds, taskID)
				}
			}
			for _, taskID := range taskIds {
				if err := p.describeVpcTaskResult(taskID); err != nil {
					p.logger.Warnf("[%s] disassociate eip task [%d] error\n", p.GetProviderName(), taskID)
				}
			}
			taskID, err := p.releaseAddresses(eipIds)
			if err != nil {
				p.logger.Warnf("[%s] release eip [%s] error\n", p.GetProviderName(), eipIds)
			}
			if err := p.describeVpcTaskResult(taskID); err != nil {
				p.logger.Warnf("[%s] release eip task [%d] error\n", p.GetProviderName(), taskID)
			}
		}

		wait.ErrWaitTimeout = fmt.Errorf("[%s] calling rollback error, please remove the cloud provider instances manually. region: %s, "+
			"instanceName: %s, msg: the maximum number of attempts reached", p.GetProviderName(), p.Region, ids)

		// retry 5 times, total 120 seconds.
		backoff := wait.Backoff{
			Duration: 30 * time.Second,
			Factor:   1,
			Steps:    5,
		}

		if err := wait.ExponentialBackoff(backoff, func() (bool, error) {
			if err := p.terminateInstances(ids); err != nil {
				return false, nil
			}
			return true, nil
		}); err != nil {
			return err
		}
	}

	p.logger.Infof("[%s] successfully executed rollback logic\n", p.GetProviderName())

	return nil
}

func (p *Tencent) DeleteK3sCluster(f bool) error {
	isConfirmed := true

	if !f {
		isConfirmed = utils.AskForConfirmation(fmt.Sprintf("[%s] are you sure to delete cluster %s", p.GetProviderName(), p.Name))
	}

	if isConfirmed {
		p.logger = common.NewLogger(common.Debug)
		p.logger.Infof("[%s] executing delete cluster logic...\n", p.GetProviderName())

		if err := p.generateClientSDK(); err != nil {
			return err
		}

		if err := p.deleteCluster(f); err != nil {
			return err
		}

		p.logger.Infof("[%s] successfully excuted delete cluster logic\n", p.GetProviderName())
	}

	return nil
}

func (p *Tencent) StartK3sCluster() error {
	p.logger = common.NewLogger(common.Debug)
	p.logger.Infof("[%s] executing start logic...\n", p.GetProviderName())

	if err := p.generateClientSDK(); err != nil {
		return err
	}

	if err := p.startCluster(); err != nil {
		return err
	}

	p.logger.Infof("[%s] successfully executed start logic\n", p.GetProviderName())

	return nil
}

func (p *Tencent) StopK3sCluster(f bool) error {
	p.logger = common.NewLogger(common.Debug)
	p.logger.Infof("[%s] executing stop logic...\n", p.GetProviderName())

	if err := p.generateClientSDK(); err != nil {
		return err
	}

	if err := p.stopCluster(f); err != nil {
		return err
	}

	p.logger.Infof("[%s] successfully executed stop logic\n", p.GetProviderName())

	return nil
}

func (p *Tencent) SSHK3sNode(ssh *types.SSH) error {
	p.logger = common.NewLogger(common.Debug)
	p.logger.Infof("[%s] executing ssh logic...\n", p.GetProviderName())

	if err := p.generateClientSDK(); err != nil {
		return err
	}

	response, err := p.describeInstances()
	if err != nil {
		return err
	}
	if len(response.Response.InstanceSet) < 1 {
		return fmt.Errorf("[%s] calling preflight error: cluster name `%s` do not exist",
			p.GetProviderName(), p.Name)
	}

	ids := make(map[string]string, len(response.Response.InstanceSet))
	for _, instance := range response.Response.InstanceSet {
		if instance.PublicIpAddresses != nil {
			for _, t := range instance.Tags {
				switch *t.Key {
				case "master":
					if *t.Value == "true" {
						ids[*instance.InstanceId] = *instance.PublicIpAddresses[0] + " (master)"
					}
				case "worker":
					if *t.Value == "true" {
						ids[*instance.InstanceId] = *instance.PublicIpAddresses[0] + " (worker)"
					}
				default:
					continue
				}
			}
		}
	}

	ip := strings.Split(utils.AskForSelectItem(fmt.Sprintf("[%s] choose ssh node to connect", p.GetProviderName()), ids), " (")[0]

	if ip == "" {
		return fmt.Errorf("[%s] choose incorrect ssh node", p.GetProviderName())
	}

	c := &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}

	// ssh K3s node.
	if err := cluster.SSHK3sNode(ip, c, ssh); err != nil {
		return err
	}

	p.logger.Infof("[%s] successfully executed ssh logic\n", p.GetProviderName())

	return nil
}

func (p *Tencent) IsClusterExist() (bool, []string, error) {
	ids := make([]string, 0)

	if p.c == nil {
		if err := p.generateClientSDK(); err != nil {
			return false, ids, err
		}
	}
	response, err := p.describeInstances()
	if err != nil {
		return false, ids, nil
	}
	if len(response.Response.InstanceSet) > 0 {
		for _, resource := range response.Response.InstanceSet {
			ids = append(ids, *resource.InstanceId)
		}
	}
	return true, ids, err
}

func (p *Tencent) GenerateMasterExtraArgs(cluster *types.Cluster, master types.Node) string {
	if option, ok := cluster.Options.(tencent.Options); ok {
		if cluster.CloudControllerManager {
			extraArgs := fmt.Sprintf(" --kubelet-arg=cloud-provider=external --kubelet-arg=node-status-update-frequency=30s --kubelet-arg=provider-id=%s.%s --node-name=%s",
				option.Region, master.InstanceID, master.InternalIPAddress[0])
			return extraArgs
		}
	}
	return ""
}

func (p *Tencent) GenerateWorkerExtraArgs(cluster *types.Cluster, worker types.Node) string {
	return p.GenerateMasterExtraArgs(cluster, worker)
}

func (p *Tencent) generateClientSDK() error {
	if p.SecretID == "" {
		p.SecretID = viper.GetString(p.GetProviderName(), secretID)
	}

	if p.SecretKey == "" {
		p.SecretKey = viper.GetString(p.GetProviderName(), secretKey)
	}

	credential := tencentCommon.NewCredential(
		p.SecretID,
		p.SecretKey,
	)
	cpf := profile.NewClientProfile()
	if p.EndpointURL != "" {
		cpf.HttpProfile.Endpoint = p.EndpointURL
	}
	if client, err := cvm.NewClient(credential, p.Region, cpf); err == nil {
		p.c = client
	} else {
		return err
	}

	if vpcClient, err := vpc.NewClient(credential, p.Region, cpf); err == nil {
		p.v = vpcClient
	} else {
		return err
	}

	// region for tag clients is not necessary
	if tagClient, err := tag.NewClient(credential, p.Region, cpf); err == nil {
		p.t = tagClient
	} else {
		return err
	}

	if tkeClient, err := tke.NewClient(credential, p.Region, cpf); err == nil {
		p.r = tkeClient
	} else {
		return err
	}
	return nil
}

func (p *Tencent) generateInstance(fn checkFun, ssh *types.SSH) (*types.Cluster, error) {
	var (
		masterEips []*vpc.Address
		workerEips []*vpc.Address
		err        error
	)

	if err = p.generateClientSDK(); err != nil {
		return nil, err
	}

	if err = fn(); err != nil {
		return nil, err
	}

	masterNum, _ := strconv.Atoi(p.Master)
	workerNum, _ := strconv.Atoi(p.Worker)

	p.logger.Debugf("[%s] %d masters and %d workers will be added\n", p.GetProviderName(), masterNum, workerNum)

	// run ecs master instances.
	if masterNum > 0 {
		if err := p.runInstances(masterNum, true); err != nil {
			return nil, err
		}
	}

	// run ecs worker instances.
	if workerNum > 0 {
		if err := p.runInstances(workerNum, false); err != nil {
			return nil, err
		}
	}

	// wait ecs instances to be running status.
	if err = p.getInstanceStatus(tencent.StatusRunning); err != nil {
		return nil, err
	}

	var (
		associatedEipIds []string
		eipTaskIds       []uint64
	)

	// allocate eip for master
	if masterNum > 0 && p.PublicIPAssignedEIP {
		p.logger.Debugf("[%s] allocating %d eip(s) for master(s)\n", p.GetProviderName(), masterNum)
		masterEipIds, taskID, err := p.allocateAddresses(masterNum)
		if err != nil {
			return nil, err
		}
		if err = p.describeVpcTaskResult(taskID); err != nil {
			p.logger.Errorf("[%s] failed to allocate eip(s) for master(s): taskId:[%d]\n", p.GetProviderName(), taskID)
			return nil, err
		}
		if masterEips, err = p.describeAddresses(masterEipIds, nil); err != nil {
			p.logger.Errorf("[%s] error when query eip info:[%s]\n", p.GetProviderName(), tencentCommon.StringValues(masterEipIds))
			return nil, err
		}
		p.logger.Debugf("[%s] successfully allocated %d eip(s) for master(s)\n", p.GetProviderName(), masterNum)
	}

	// associate master eip
	if masterEips != nil {
		p.logger.Debugf("[%s] associating %d eip(s) for master(s)\n", p.GetProviderName(), masterNum)

		j := 0
		associatedEipInfo := make(map[string]string)
		for i, master := range p.Status.MasterNodes {
			if p.Status.MasterNodes[i].PublicIPAddress == nil {
				p.Status.MasterNodes[i].EipAllocationIds = append(p.Status.MasterNodes[i].EipAllocationIds, *masterEips[j].AddressId)
				p.Status.MasterNodes[i].PublicIPAddress = append(p.Status.MasterNodes[i].PublicIPAddress, *masterEips[j].AddressIp)
				associatedEipInfo[master.InstanceID] = *masterEips[j].AddressId
				associatedEipIds = append(associatedEipIds, *masterEips[j].AddressId)
				j++
			}
		}
		for instanceID, allocationID := range associatedEipInfo {
			taskID, err := p.associateAddress(allocationID, instanceID)
			if err != nil {
				return nil, err
			}
			eipTaskIds = append(eipTaskIds, taskID)
		}
		p.logger.Debugf("[%s] successfully associated %d eip(s) for master(s)\n", p.GetProviderName(), masterNum)
	}

	// allocate eip for worker
	if workerNum > 0 && p.PublicIPAssignedEIP {
		p.logger.Debugf("[%s] allocating %d eip(s) for worker(s)\n", p.GetProviderName(), workerNum)
		workerEipIds, taskID, err := p.allocateAddresses(workerNum)
		if err != nil {
			return nil, err
		}
		if err = p.describeVpcTaskResult(taskID); err != nil {
			p.logger.Errorf("[%s] failed to allocate eip(s) for master(s): taskId:[%d]\n", p.GetProviderName(), taskID)
			return nil, err
		}
		if workerEips, err = p.describeAddresses(workerEipIds, nil); err != nil {
			p.logger.Errorf("[%s] error when query eip info:[%s]\n", p.GetProviderName(), tencentCommon.StringValues(workerEipIds))
			return nil, err
		}
		p.logger.Debugf("[%s] successfully allocated %d eip(s) for worker(s)\n", p.GetProviderName(), workerNum)
	}

	// associate worker eip
	if workerEips != nil {
		p.logger.Debugf("[%s] associating %d eip(s) for worker(s)\n", p.GetProviderName(), workerNum)

		j := 0
		associatedEipInfo := make(map[string]string)
		for i, worker := range p.Status.WorkerNodes {
			if p.Status.WorkerNodes[i].PublicIPAddress == nil {
				p.Status.WorkerNodes[i].EipAllocationIds = append(p.Status.WorkerNodes[i].EipAllocationIds, *workerEips[j].AddressId)
				p.Status.WorkerNodes[i].PublicIPAddress = append(p.Status.WorkerNodes[i].PublicIPAddress, *workerEips[j].AddressIp)
				associatedEipInfo[worker.InstanceID] = *workerEips[j].AddressId
				associatedEipIds = append(associatedEipIds, *workerEips[j].AddressId)
				j++
			}
		}
		for instanceID, allocationID := range associatedEipInfo {
			taskID, err := p.associateAddress(allocationID, instanceID)
			if err != nil {
				return nil, err
			}
			eipTaskIds = append(eipTaskIds, taskID)
		}
		p.logger.Debugf("[%s] successfully associated %d eip(s) for worker(s)\n", p.GetProviderName(), workerNum)
	}

	// wait eip to be InUse status.
	if p.PublicIPAssignedEIP {
		for _, taskID := range eipTaskIds {
			if err = p.describeVpcTaskResult(taskID); err != nil {
				return nil, err
			}
		}
	}

	// assemble instance status.
	var c *types.Cluster
	if c, err = p.assembleInstanceStatus(ssh); err != nil {
		return nil, err
	}

	c.InstallScript = k3sScript
	c.Mirror = k3sMirror
	c.DockerMirror = dockerMirror

	if _, ok := c.Options.(tencent.Options); ok {
		if c.CloudControllerManager {
			c.MasterExtraArgs += " --disable-cloud-controller --no-deploy servicelb,traefik"
		}
	}

	return c, nil
}

func (p *Tencent) startCluster() error {
	exist, ids, err := p.IsClusterExist()

	if !exist {
		return fmt.Errorf("[%s] calling preflight error: cluster name `%s` do not exist",
			p.GetProviderName(), p.Name)
	}

	if err == nil && len(ids) > 0 {
		// ensure that the status of all instances is stopped.
		if err := p.startAndStopCheck(tencent.StatusStopped); err != nil {
			return err
		}

		if err := p.startInstances(ids); err != nil {
			return fmt.Errorf("[%s] calling startInstance error, msg: [%v]", p.GetProviderName(), err)
		}
	}

	// wait ecs instances to be running status.
	if err = p.getInstanceStatus(tencent.StatusRunning); err != nil {
		return err
	}

	p.Status.Status = common.StatusRunning

	err = cluster.SaveState(&types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	})

	if err != nil {
		return fmt.Errorf("[%s] synchronizing .state file error, msg: [%v]", p.GetProviderName(), err)
	}
	return nil
}

func (p *Tencent) stopCluster(f bool) error {
	exist, ids, err := p.IsClusterExist()

	if !exist {
		return fmt.Errorf("[%s] calling preflight error: cluster name `%s` do not exist", p.GetProviderName(), p.Name)
	}

	if err == nil && len(ids) > 0 {
		// ensure that the status of all instances is running.
		if err := p.startAndStopCheck(tencent.Running); err != nil && !f {
			return err
		}

		if err = p.stopInstances(ids, f); err != nil && !f {
			return err
		}
	}

	// wait ecs instances to be stopped status.
	if err = p.getInstanceStatus(tencent.StatusStopped); err != nil {
		return err
	}

	p.Status.Status = common.StatusStopped

	err = cluster.SaveState(&types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	})

	if err != nil {
		return fmt.Errorf("[%s] synchronizing .state file error, msg: [%v]", p.GetProviderName(), err)
	}

	return nil
}

func (p *Tencent) deleteCluster(f bool) error {
	exist, ids, err := p.IsClusterExist()

	if err != nil {
		return err
	}

	if !exist && !f {
		return fmt.Errorf("[%s] calling preflight error: cluster name `%s` do not exist", p.GetProviderName(), p.Name)
	}

	taggedResource, err := p.describeResourcesByTags()
	if err != nil {
		p.logger.Errorf("[%s] error when query tagged eip(s), message: %v", p.GetProviderName(), err)
	}
	var eipIds []string
	if len(taggedResource) > 0 {
		var taskIds []uint64
		for _, resource := range taggedResource {
			if strings.EqualFold(tencent.ServiceTypeEIP, *resource.ServiceType) &&
				strings.EqualFold(tencent.ResourcePrefixEIP, *resource.ResourcePrefix) {
				eipID := *resource.ResourceId
				if eipID != "" {
					taskID, err := p.disassociateAddress(eipID)
					if err != nil {
						p.logger.Errorf("[%s] error when query task eip disassociate progress, message: %v", p.GetProviderName(), err)
					}
					eipIds = append(eipIds, eipID)
					taskIds = append(taskIds, taskID)
				}
			}
		}
		for _, taskID := range taskIds {
			if err := p.describeVpcTaskResult(taskID); err != nil {
				p.logger.Errorf("[%s] error when query eip disassociate task result, message: %v", p.GetProviderName(), err)
			}
		}
	} else {
		p.logger.Warnf("[%s] failed to query tagged eip", p.GetProviderName())
	}
	if len(eipIds) > 0 {
		taskID, err := p.releaseAddresses(eipIds)
		if err != nil {
			p.logger.Errorf("[%s] failed to release tagged eip, message: %v", p.GetProviderName(), err)
		}
		if err := p.describeVpcTaskResult(taskID); err != nil {
			p.logger.Errorf("[%s] failed to query release eip task result, message: %v", p.GetProviderName(), err)
		}
	}

	if err == nil && len(ids) > 0 {
		p.logger.Debugf("[%s] cluster %s will be deleted\n", p.GetProviderName(), p.Name)

		err := p.terminateInstances(ids)
		if err != nil {
			return fmt.Errorf("[%s] calling deleteInstance error, msg: %v", p.GetProviderName(), err)
		}
	}

	if err != nil && !f {
		return fmt.Errorf("[%s] calling deleteInstance error, msg: %v", p.GetProviderName(), err)
	}

	err = cluster.OverwriteCfg(p.Name)

	if err != nil && !f {
		return fmt.Errorf("[%s] synchronizing .cfg file error, msg: %v", p.GetProviderName(), err)
	}

	err = cluster.DeleteState(p.Name, p.Provider)

	if err != nil && !f {
		return fmt.Errorf("[%s] synchronizing .state file error, msg: %v", p.GetProviderName(), err)
	}

	p.logger.Debugf("[%s] successfully deleted cluster %s\n", p.GetProviderName(), p.Name)

	return nil
}

func (p *Tencent) createCheck() error {
	masterNum, _ := strconv.Atoi(p.Master)
	if masterNum < 1 {
		return fmt.Errorf("[%s] calling preflight error: `--master` number must >= 1",
			p.GetProviderName())
	}

	if strings.Contains(p.MasterExtraArgs, "--datastore-endpoint") && p.DataStore != "" {
		return fmt.Errorf("[%s] calling preflight error: `--masterExtraArgs='--datastore-endpoint'` is duplicated with `--datastore`",
			p.GetProviderName())
	}

	exist, _, err := p.IsClusterExist()
	if err != nil {
		return err
	}

	if exist {
		return fmt.Errorf("[%s] calling preflight error: cluster name `%s` already exist",
			p.GetProviderName(), p.Name)
	}

	return nil
}

func (p *Tencent) joinCheck() error {
	if strings.Contains(p.MasterExtraArgs, "--datastore-endpoint") && p.DataStore != "" {
		return fmt.Errorf("[%s] calling preflight error: `--masterExtraArgs='--datastore-endpoint'` is duplicated with `--datastore`",
			p.GetProviderName())
	}

	exist, ids, err := p.IsClusterExist()

	if err != nil {
		return err
	}

	if !exist {
		return fmt.Errorf("[%s] calling preflight error: cluster name `%s` do not exist",
			p.GetProviderName(), p.Name)
	}

	// remove invalid worker nodes from .state file.
	workers := make([]types.Node, 0)
	for _, w := range p.WorkerNodes {
		for _, e := range ids {
			if e == w.InstanceID {
				workers = append(workers, w)
				break
			}
		}
	}
	p.WorkerNodes = workers

	return nil
}

func (p *Tencent) startAndStopCheck(aimStatus string) error {
	response, err := p.describeInstances()
	if err != nil {
		return err
	}
	if len(response.Response.InstanceSet) > 0 {
		masterCnt := 0
		unexpectedStatusCnt := 0
		for _, instance := range response.Response.InstanceSet {
			instanceID := *instance.InstanceId
			instanceState := *instance.InstanceState
			if instanceState != aimStatus {
				unexpectedStatusCnt++
				p.logger.Warnf("[%s] instance [%s] status is %s, but it is expected to be %s\n",
					p.GetProviderName(), instanceID, instanceState, aimStatus)
			}
			master := false
			for _, tagPtr := range instance.Tags {
				if strings.EqualFold(*tagPtr.Key, "master") && strings.EqualFold(*tagPtr.Value, "true") {
					master = true
					masterCnt++
					break
				}
			}
			var eip []string
			if p.PublicIPAssignedEIP {
				eipInfos, err := p.describeAddresses(nil, []*string{instance.InstanceId})
				if err != nil {
					p.logger.Errorf("[%s] error when query eip info of instance:[%s]\n", p.GetProviderName(), *instance.InstanceId)
					return err
				}
				for _, eipInfo := range eipInfos {
					eip = append(eip, *eipInfo.AddressId)
				}
			}
			p.m.Store(instanceID, types.Node{
				Master:            master,
				InstanceID:        instanceID,
				InstanceStatus:    instanceState,
				InternalIPAddress: tencentCommon.StringValues(instance.PrivateIpAddresses),
				PublicIPAddress:   tencentCommon.StringValues(instance.PublicIpAddresses),
				EipAllocationIds:  eip,
			})
		}
		if unexpectedStatusCnt > 0 {
			return fmt.Errorf("[%s] status of %d instance(s) is unexpected", p.GetProviderName(), unexpectedStatusCnt)
		}
		p.Master = strconv.Itoa(masterCnt)
		p.Worker = strconv.Itoa(len(response.Response.InstanceSet) - masterCnt)
		return nil
	}
	return fmt.Errorf("[%s] unable to confirm the current status of instance(s)", p.GetProviderName())
}

func (p *Tencent) assembleInstanceStatus(ssh *types.SSH) (*types.Cluster, error) {
	response, err := p.describeInstances()
	if err != nil {
		return nil, err
	}

	for _, status := range response.Response.InstanceSet {
		InstanceID := *status.InstanceId
		var eip []string
		if p.PublicIPAssignedEIP {
			eipInfos, err := p.describeAddresses(nil, []*string{status.InstanceId})
			if err != nil {
				p.logger.Errorf("[%s] error when query eip info of instance:[%s]\n", p.GetProviderName(), *status.InstanceId)
				return nil, err
			}
			for _, eipInfo := range eipInfos {
				eip = append(eip, *eipInfo.AddressId)
			}
		}
		if value, ok := p.m.Load(InstanceID); ok {
			v := value.(types.Node)
			// add only nodes that run the current command.
			v.Current = true
			v.InternalIPAddress = tencentCommon.StringValues(status.PrivateIpAddresses)
			v.PublicIPAddress = tencentCommon.StringValues(status.PublicIpAddresses)
			v.EipAllocationIds = eip
			p.m.Store(InstanceID, v)
			continue
		}

		master := false
		for _, tagPtr := range status.Tags {
			if strings.EqualFold(*tagPtr.Key, "master") && strings.EqualFold(*tagPtr.Value, "true") {
				master = true
				break
			}
		}

		if master {
			p.m.Store(InstanceID, types.Node{
				Master:            true,
				RollBack:          false,
				InstanceID:        InstanceID,
				InstanceStatus:    tencent.StatusRunning,
				InternalIPAddress: tencentCommon.StringValues(status.PrivateIpAddresses),
				EipAllocationIds:  eip,
				PublicIPAddress:   tencentCommon.StringValues(status.PublicIpAddresses)})
		} else {
			p.m.Store(InstanceID, types.Node{
				Master:            false,
				RollBack:          false,
				InstanceID:        InstanceID,
				InstanceStatus:    tencent.StatusRunning,
				InternalIPAddress: tencentCommon.StringValues(status.PrivateIpAddresses),
				EipAllocationIds:  eip,
				PublicIPAddress:   tencentCommon.StringValues(status.PublicIpAddresses)})
		}
	}

	p.m.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		v.SSH = *ssh
		if v.Master {
			index := -1
			for i, n := range p.Status.MasterNodes {
				if n.InstanceID == v.InstanceID {
					index = i
					break
				}
			}
			if index > -1 {
				p.Status.MasterNodes[index] = v
			} else {
				p.Status.MasterNodes = append(p.Status.MasterNodes, v)
			}
		} else {
			index := -1
			for i, n := range p.Status.WorkerNodes {
				if n.InstanceID == v.InstanceID {
					index = i
					break
				}
			}
			if index > -1 {
				p.Status.WorkerNodes[index] = v
			} else {
				p.Status.WorkerNodes = append(p.Status.WorkerNodes, v)
			}
		}
		return true
	})

	return &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}, nil
}

func (p *Tencent) runInstances(num int, master bool) error {
	if master {
		p.logger.Debugf("[%s] %d number of master instances will be created\n", p.GetProviderName(), num)
	} else {
		p.logger.Debugf("[%s] %d number of worker instances will be created\n", p.GetProviderName(), num)
	}

	request := cvm.NewRunInstancesRequest()

	diskSize, _ := strconv.ParseInt(p.SystemDiskSize, 10, 64)
	bandwidth, _ := strconv.ParseInt(p.InternetMaxBandwidthOut, 10, 64)

	request.InstanceCount = tencentCommon.Int64Ptr(int64(num))
	request.ImageId = tencentCommon.StringPtr(p.ImageID)
	request.InstanceType = tencentCommon.StringPtr(p.InstanceType)
	request.Placement = &cvm.Placement{
		Zone: tencentCommon.StringPtr(p.Zone),
	}
	request.InstanceChargeType = tencentCommon.StringPtr(instanceChargeType)
	request.SecurityGroupIds = tencentCommon.StringPtrs(strings.Split(p.SecurityGroupIds, ","))
	request.VirtualPrivateCloud = &cvm.VirtualPrivateCloud{
		SubnetId: tencentCommon.StringPtr(p.SubnetID),
		VpcId:    tencentCommon.StringPtr(p.VpcID),
	}
	request.SystemDisk = &cvm.SystemDisk{
		DiskType: tencentCommon.StringPtr(p.SystemDiskType),
		DiskSize: tencentCommon.Int64Ptr(diskSize),
	}
	loginSettings := &cvm.LoginSettings{}
	if p.Password != "" {
		loginSettings.Password = &p.Password
	}
	if p.KeyIds != "" {
		// only support bind one though it's array
		loginSettings.KeyIds = tencentCommon.StringPtrs([]string{p.KeyIds})
	}
	request.LoginSettings = loginSettings
	request.InternetAccessible = &cvm.InternetAccessible{
		InternetChargeType:      tencentCommon.StringPtr(internetChargeType),
		InternetMaxBandwidthOut: tencentCommon.Int64Ptr(bandwidth),
		PublicIpAssigned:        tencentCommon.BoolPtr(!p.PublicIPAssignedEIP),
	}

	// tags
	tags := []*cvm.Tag{
		{Key: tencentCommon.StringPtr("autok3s"), Value: tencentCommon.StringPtr("true")},
		{Key: tencentCommon.StringPtr("cluster"), Value: tencentCommon.StringPtr(common.TagClusterPrefix + p.Name)},
	}
	if master {
		request.InstanceName = tencentCommon.StringPtr(fmt.Sprintf(common.MasterInstanceName, p.Name))
		tags = append(tags, &cvm.Tag{Key: tencentCommon.StringPtr("master"), Value: tencentCommon.StringPtr("true")})
	} else {
		request.InstanceName = tencentCommon.StringPtr(fmt.Sprintf(common.WorkerInstanceName, p.Name))
		tags = append(tags, &cvm.Tag{Key: tencentCommon.StringPtr("worker"), Value: tencentCommon.StringPtr("true")})
	}
	request.TagSpecification = []*cvm.TagSpecification{{ResourceType: tencentCommon.StringPtr("instance"), Tags: tags}}

	response, err := p.c.RunInstances(request)
	if err != nil || len(response.Response.InstanceIdSet) != num {
		return fmt.Errorf("[%s] calling runInstances error. region: %s, zone: %s, "+"instanceName: %s, msg: [%v]",
			p.GetProviderName(), p.Region, p.Zone, *request.InstanceName, err)
	}
	for _, id := range response.Response.InstanceIdSet {
		p.m.Store(*id, types.Node{Master: master, RollBack: true, InstanceID: *id, InstanceStatus: tencent.StatusPending})
	}

	if master {
		p.logger.Debugf("[%s] %d number of master instances successfully created\n", p.GetProviderName(), num)
	} else {
		p.logger.Debugf("[%s] %d number of worker instances successfully created\n", p.GetProviderName(), num)
	}
	return nil
}

func (p *Tencent) describeInstances() (*cvm.DescribeInstancesResponse, error) {
	request := cvm.NewDescribeInstancesRequest()

	request.Limit = tencentCommon.Int64Ptr(maxPageSize)
	// If there are multiple Filters, between the Filters is a logical AND (AND)
	// If there are multiple Values in the same Filter, between Values under the same Filter is a logical OR (OR)
	request.Filters = []*cvm.Filter{
		{Name: tencentCommon.StringPtr("tag:autok3s"), Values: tencentCommon.StringPtrs([]string{"true"})},
		{Name: tencentCommon.StringPtr("tag:cluster"), Values: tencentCommon.StringPtrs([]string{common.TagClusterPrefix + p.Name})},
	}
	response, err := p.c.DescribeInstances(request)
	if err == nil && len(response.Response.InstanceSet) == 0 {
		return nil, fmt.Errorf("[%s] calling describeInstances error. region: %s, zone: %s, "+"cluster: %s",
			p.GetProviderName(), p.Region, p.Zone, p.Name)
	}
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (p *Tencent) terminateInstances(instanceIds []string) error {
	request := cvm.NewTerminateInstancesRequest()

	request.InstanceIds = tencentCommon.StringPtrs(instanceIds)

	_, err := p.c.TerminateInstances(request)

	if err != nil {
		return fmt.Errorf("[%s] calling deleteInstance error, msg: %v", p.GetProviderName(), err)
	}

	return nil
}

func (p *Tencent) startInstances(instanceIds []string) error {
	request := cvm.NewStartInstancesRequest()

	request.InstanceIds = tencentCommon.StringPtrs(instanceIds)

	_, err := p.c.StartInstances(request)

	if err != nil {
		return fmt.Errorf("[%s] calling startInstances error, msg: %v", p.GetProviderName(), err)
	}

	return nil
}

func (p *Tencent) stopInstances(instanceIds []string, force bool) error {
	request := cvm.NewStopInstancesRequest()

	request.InstanceIds = tencentCommon.StringPtrs(instanceIds)
	request.ForceStop = tencentCommon.BoolPtr(force)

	_, err := p.c.StopInstances(request)

	if err != nil {
		return fmt.Errorf("[%s] calling stopInstances error, msg: %v", p.GetProviderName(), err)
	}

	return nil
}

func (p *Tencent) allocateAddresses(num int) ([]*string, uint64, error) {
	request := vpc.NewAllocateAddressesRequest()

	request.AddressCount = tencentCommon.Int64Ptr(int64(num))
	request.InternetChargeType = tencentCommon.StringPtr(internetChargeType)
	internetMaxBandwidthOut, _ := strconv.ParseInt(p.InternetMaxBandwidthOut, 10, 64)
	request.InternetMaxBandwidthOut = tencentCommon.Int64Ptr(internetMaxBandwidthOut)
	request.Tags = []*vpc.Tag{
		{Key: tencentCommon.StringPtr("autok3s"), Value: tencentCommon.StringPtr("true")},
		{Key: tencentCommon.StringPtr("cluster"), Value: tencentCommon.StringPtr(common.TagClusterPrefix + p.Name)},
	}
	response, err := p.v.AllocateAddresses(request)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] calling allocateAddresses error, msg: %v", p.GetProviderName(), err)
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return response.Response.AddressSet, taskID, nil
}

func (p *Tencent) releaseAddresses(addressIds []string) (uint64, error) {
	request := vpc.NewReleaseAddressesRequest()

	request.AddressIds = tencentCommon.StringPtrs(addressIds)

	response, err := p.v.ReleaseAddresses(request)

	if err != nil {
		return 0, fmt.Errorf("[%s] calling releaseAddresses error, msg: %v", p.GetProviderName(), err)
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return taskID, nil
}

func (p *Tencent) describeAddresses(addressIds, instanceIds []*string) ([]*vpc.Address, error) {
	request := vpc.NewDescribeAddressesRequest()

	if len(instanceIds) <= 0 {
		request.AddressIds = addressIds
	} else {
		filters := []*vpc.Filter{
			{Name: tencentCommon.StringPtr("instance-id"), Values: instanceIds},
		}
		if len(addressIds) > 0 {
			filters = append(filters, &vpc.Filter{Name: tencentCommon.StringPtr("address-id"), Values: addressIds})
		}
		request.Filters = filters
	}

	response, err := p.v.DescribeAddresses(request)

	if err != nil {
		return nil, fmt.Errorf("[%s] calling describeAddresses error, msg: %v", p.GetProviderName(), err)
	}

	return response.Response.AddressSet, nil
}

func (p *Tencent) associateAddress(addressID, instanceID string) (uint64, error) {
	request := vpc.NewAssociateAddressRequest()

	request.AddressId = tencentCommon.StringPtr(addressID)
	request.InstanceId = tencentCommon.StringPtr(instanceID)

	response, err := p.v.AssociateAddress(request)

	if err != nil {
		return 0, fmt.Errorf("[%s] calling associateAddress error, msg: %v", p.GetProviderName(), err)
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return taskID, nil
}

func (p *Tencent) disassociateAddress(addressID string) (uint64, error) {
	request := vpc.NewDisassociateAddressRequest()

	request.AddressId = tencentCommon.StringPtr(addressID)

	response, err := p.v.DisassociateAddress(request)

	if err != nil {
		return 0, fmt.Errorf("[%s] calling associateAddress error, msg: %v", p.GetProviderName(), err)
	}
	taskID, err := strconv.ParseUint(*response.Response.TaskId, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("[%s] error when convert taskID: %s", p.GetProviderName(), *response.Response.TaskId)
	}
	return taskID, nil
}

func (p *Tencent) describeVpcTaskResult(taskID uint64) error {
	request := vpc.NewDescribeTaskResultRequest()
	request.TaskId = tencentCommon.Uint64Ptr(taskID)

	wait.ErrWaitTimeout = fmt.Errorf("[%s] calling describeVpcTaskResult error. region: %s, zone: %s, taskId: %d",
		p.GetProviderName(), p.Region, p.Zone, taskID)

	if err := wait.ExponentialBackoff(common.Backoff, func() (bool, error) {
		response, err := p.v.DescribeTaskResult(request)
		if err != nil {
			return false, nil
		}

		switch strings.ToUpper(*response.Response.Result) {
		case tencent.Running:
			return false, nil
		case tencent.Success:
			return true, nil
		case tencent.Failed:
			return true, fmt.Errorf("[%s] task failed %d", p.GetProviderName(), taskID)
		}

		return true, nil
	}); err != nil {
		return err
	}
	return nil
}

func (p *Tencent) getInstanceStatus(aimStatus string) error {
	ids := make([]string, 0)
	p.m.Range(func(key, value interface{}) bool {
		ids = append(ids, key.(string))
		return true
	})

	if len(ids) > 0 {
		p.logger.Debugf("[%s] waiting for the instances %s to be in `%s` status...\n", p.GetProviderName(), ids, aimStatus)
		request := cvm.NewDescribeInstancesStatusRequest()
		request.InstanceIds = tencentCommon.StringPtrs(ids)

		wait.ErrWaitTimeout = fmt.Errorf("[%s] calling getInstanceStatus error. region: %s, zone: %s, instanceName: %s, message: not `%s` status",
			p.GetProviderName(), p.Region, p.Zone, ids, aimStatus)

		if err := wait.ExponentialBackoff(common.Backoff, func() (bool, error) {
			response, err := p.c.DescribeInstancesStatus(request)
			if err != nil || len(response.Response.InstanceStatusSet) <= 0 {
				return false, nil
			}

			for _, status := range response.Response.InstanceStatusSet {
				if *status.InstanceState == aimStatus {
					instanceID := *status.InstanceId
					if value, ok := p.m.Load(instanceID); ok {
						v := value.(types.Node)
						v.InstanceStatus = aimStatus
						p.m.Store(instanceID, v)
					}
				} else {
					return false, nil
				}
			}

			return true, nil
		}); err != nil {
			return err
		}
	}

	p.m.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		if v.Master {
			index := -1
			for i, n := range p.Status.MasterNodes {
				if n.InstanceID == v.InstanceID {
					index = i
					break
				}
			}
			if index > -1 {
				p.Status.MasterNodes[index] = v
			} else {
				p.Status.MasterNodes = append(p.Status.MasterNodes, v)
			}
		} else {
			index := -1
			for i, n := range p.Status.WorkerNodes {
				if n.InstanceID == v.InstanceID {
					index = i
					break
				}
			}
			if index > -1 {
				p.Status.WorkerNodes[index] = v
			} else {
				p.Status.WorkerNodes = append(p.Status.WorkerNodes, v)
			}
		}
		return true
	})

	p.logger.Debugf("[%s] instances %s are in `%s` status\n", p.GetProviderName(), ids, aimStatus)

	return nil
}

func (p *Tencent) describeResourcesByTags() ([]*tag.ResourceTag, error) {
	request := tag.NewDescribeResourcesByTagsRequest()

	request.TagFilters = []*tag.TagFilter{
		{TagKey: tencentCommon.StringPtr("autok3s"), TagValue: tencentCommon.StringPtrs([]string{"true"})},
		{TagKey: tencentCommon.StringPtr("cluster"), TagValue: tencentCommon.StringPtrs([]string{common.TagClusterPrefix + p.Name})},
	}

	response, err := p.t.DescribeResourcesByTags(request)
	if err != nil {
		return nil, err
	}
	return response.Response.Rows, err
}
