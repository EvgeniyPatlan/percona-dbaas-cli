// Copyright © 2019 Percona, LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pxc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Percona-Lab/percona-dbaas-cli/dbaas"
	"github.com/Percona-Lab/percona-dbaas-cli/dbaas/pxc"
)

type Version string

const (
	CurrentVersion Version = "default"

	defaultOperatorVersion = "percona/percona-xtradb-cluster-operator:1.1.0"
)

type PXC struct {
	name         string
	config       *pxc.PerconaXtraDBCluster
	obj          dbaas.Objects
	dbpass       []byte
	opLogsLastTS float64
}

func NewPXC(name string, version pxc.Version) *PXC {
	return &PXC{
		name:   name,
		obj:    pxc.Objects[version],
		config: &pxc.PerconaXtraDBCluster{},
	}
}

func (p PXC) Bundle(operatorVersion string) []dbaas.BundleObject {
	if operatorVersion == "" {
		operatorVersion = defaultOperatorVersion
	}

	for i, o := range p.obj.Bundle {
		if o.Kind == "Deployment" && o.Name == p.OperatorName() {
			p.obj.Bundle[i].Data = strings.Replace(o.Data, "{{image}}", operatorVersion, -1)
		}
	}
	return p.obj.Bundle
}

func (p PXC) Name() string {
	return p.name
}

func (p PXC) App() (string, error) {
	cr, err := json.Marshal(p.config)
	if err != nil {
		return "", errors.Wrap(err, "marshal cr template")
	}

	return string(cr), nil
}

const createMsg = `Create MySQL cluster.
 
PXC instances           | %v
ProxySQL instances      | %v
Storage                 | %v
`

func (p *PXC) SetNew(clusterName string, c Config, s3 *dbaas.BackupStorageSpec, platform dbaas.PlatformType) (err error) {
	p.config.ObjectMeta.Name = clusterName
	p.config.SetDefaults()

	volSizeFlag := c.StorageSize
	volSize, err := resource.ParseQuantity(volSizeFlag)
	if err != nil {
		return errors.Wrap(err, "storage-size")
	}
	p.config.Spec.PXC.VolumeSpec.PersistentVolumeClaim.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: volSize}

	volClassNameFlag := c.StorageClass

	if volClassNameFlag != "" {
		p.config.Spec.PXC.VolumeSpec.PersistentVolumeClaim.StorageClassName = &volClassNameFlag
	}

	p.config.Spec.PXC.Size = c.Instances

	pxcCPU := c.RequestCPU
	_, err = resource.ParseQuantity(pxcCPU)
	if err != nil {
		return errors.Wrap(err, "pxc-request-cpu")
	}
	pxcMemory := c.RequestMem
	_, err = resource.ParseQuantity(pxcMemory)
	if err != nil {
		return errors.Wrap(err, "pxc-request-mem")
	}
	p.config.Spec.PXC.Resources = &pxc.PodResources{
		Requests: &pxc.ResourcesList{
			CPU:    pxcCPU,
			Memory: pxcMemory,
		},
	}

	pxctpk := c.AntiAffinityKey

	if _, ok := pxc.AffinityValidTopologyKeys[pxctpk]; !ok {
		return errors.Errorf("invalid `pxc-anti-affinity-key` value: %s", pxctpk)
	}
	p.config.Spec.PXC.Affinity.TopologyKey = &pxctpk

	p.config.Spec.ProxySQL.Size = c.ProxyInstances

	// Disable ProxySQL if size set to 0
	if p.config.Spec.ProxySQL.Size > 0 {
		err := p.setProxySQL(c)
		if err != nil {
			return err
		}
	} else {
		p.config.Spec.ProxySQL.Enabled = false
	}

	if s3 != nil {
		p.config.Spec.Backup.Storages = map[string]*dbaas.BackupStorageSpec{
			dbaas.DefaultBcpStorageName: s3,
		}
	}

	switch platform {
	case dbaas.PlatformMinishift, dbaas.PlatformMinikube:
		none := pxc.AffinityTopologyKeyOff
		p.config.Spec.PXC.Affinity.TopologyKey = &none
		p.config.Spec.PXC.Resources = nil
		p.config.Spec.ProxySQL.Affinity.TopologyKey = &none
		p.config.Spec.ProxySQL.Resources = nil
	}
	return nil
}

func (p *PXC) setProxySQL(c Config) error {
	proxyCPU := c.ProxyRequestCPU
	_, err := resource.ParseQuantity(proxyCPU)
	if err != nil {
		return errors.Wrap(err, "proxy-request-cpu")
	}
	proxyMemory := c.ProxyRequestMem
	_, err = resource.ParseQuantity(proxyMemory)
	if err != nil {
		return errors.Wrap(err, "proxy-request-mem")
	}
	p.config.Spec.ProxySQL.Resources = &pxc.PodResources{
		Requests: &pxc.ResourcesList{
			CPU:    proxyCPU,
			Memory: proxyMemory,
		},
	}

	proxytpk := c.ProxyAntiAffinityKey
	if _, ok := pxc.AffinityValidTopologyKeys[proxytpk]; !ok {
		return errors.Errorf("invalid `proxy-anti-affinity-key` value: %s", proxytpk)
	}
	p.config.Spec.ProxySQL.Affinity.TopologyKey = &proxytpk

	return nil
}

func (p *PXC) Setup(c Config, s3 *dbaas.BackupStorageSpec) (string, error) {
	err := p.SetNew(p.Name(), c, s3, dbaas.GetPlatformType())
	if err != nil {
		return "", errors.Wrap(err, "parse options")
	}

	storage, err := p.config.Spec.PXC.VolumeSpec.PersistentVolumeClaim.Resources.Requests[corev1.ResourceStorage].MarshalJSON()
	if err != nil {
		return "", errors.Wrap(err, "marshal pxc volume requests")
	}

	return fmt.Sprintf(createMsg, p.config.Spec.PXC.Size, p.config.Spec.ProxySQL.Size, string(storage)), nil
}

const updateMsg = `Update MySQL cluster.
 
PXC instances           | %v
ProxySQL instances      | %v
`

func (p *PXC) Edit(crRaw []byte, f *pflag.FlagSet, storage *dbaas.BackupStorageSpec) (string, error) {
	cr := &pxc.PerconaXtraDBCluster{}
	err := json.Unmarshal(crRaw, cr)
	if err != nil {
		return "", errors.Wrap(err, "unmarshal current cr")
	}

	p.config.APIVersion = cr.APIVersion
	p.config.Kind = cr.Kind
	p.config.Name = cr.Name
	p.config.Finalizers = cr.Finalizers
	p.config.Spec = cr.Spec
	p.config.Status = cr.Status

	err = p.config.UpdateWith(f, storage)
	if err != nil {
		return "", errors.Wrap(err, "applay changes to cr")
	}

	return fmt.Sprintf(updateMsg, p.config.Spec.PXC.Size, p.config.Spec.ProxySQL.Size), nil
}

func (p *PXC) Upgrade(crRaw []byte, newImages map[string]string) error {
	cr := &pxc.PerconaXtraDBCluster{}
	err := json.Unmarshal(crRaw, cr)
	if err != nil {
		return errors.Wrap(err, "unmarshal current cr")
	}

	p.config.APIVersion = cr.APIVersion
	p.config.Kind = cr.Kind
	p.config.Name = cr.Name
	p.config.Finalizers = cr.Finalizers
	p.config.Spec = cr.Spec
	p.config.Status = cr.Status

	p.config.Upgrade(newImages)

	return nil
}

const operatorImage = "percona/percona-xtradb-cluster-operator:"

func (p *PXC) Images(ver string, f *pflag.FlagSet) (operator string, apps map[string]string, err error) {
	apps = make(map[string]string)

	if ver != "" {
		operator = operatorImage + ver
		apps["pxc"] = operatorImage + ver + "-pxc"
		apps["proxysql"] = operatorImage + ver + "-proxysql"
		apps["backup"] = operatorImage + ver + "-backup"
	}

	op, err := f.GetString("operator-image")
	if err != nil {
		return operator, apps, errors.New("undefined `operator-image`")
	}
	if op != "" {
		operator = op
	}

	pxc, err := f.GetString("pxc-image")
	if err != nil {
		return operator, apps, errors.New("undefined `pxc-image`")
	}
	if pxc != "" {
		apps["pxc"] = pxc
	}

	proxysql, err := f.GetString("proxysql-image")
	if err != nil {
		return operator, apps, errors.New("undefined `proxysql-image`")
	}
	if proxysql != "" {
		apps["proxysql"] = proxysql
	}

	backup, err := f.GetString("backup-image")
	if err != nil {
		return operator, apps, errors.New("undefined `backup-image`")
	}
	if backup != "" {
		apps["backup"] = backup
	}

	return operator, apps, nil
}

func (p *PXC) OperatorName() string {
	return "percona-xtradb-cluster-operator"
}

type k8sStatus struct {
	Status pxc.PerconaXtraDBClusterStatus
}

const okmsg = `
MySQL cluster started successfully, right endpoint for application:
Host: %s
Port: 3306
User: root
Pass: %s

Enjoy!`

func (p *PXC) CheckStatus(data []byte, pass map[string][]byte) (dbaas.ClusterState, []string, error) {
	st := &k8sStatus{}

	err := json.Unmarshal(data, st)
	if err != nil {
		return dbaas.ClusterStateUnknown, nil, errors.Wrap(err, "unmarshal status")
	}

	switch st.Status.Status {
	case pxc.AppStateReady:
		return dbaas.ClusterStateReady, []string{fmt.Sprintf(okmsg, st.Status.Host, pass["root"])}, nil
	case pxc.AppStateInit:
		return dbaas.ClusterStateInit, nil, nil
	case pxc.AppStateError:
		return dbaas.ClusterStateError, alterStatusMgs(st.Status.Messages), nil
	}

	return dbaas.ClusterStateInit, nil, nil
}

type operatorLog struct {
	Level      string  `json:"level"`
	TS         float64 `json:"ts"`
	Msg        string  `json:"msg"`
	Error      string  `json:"error"`
	Request    string  `json:"Request"`
	Controller string  `json:"Controller"`
}

func (p *PXC) CheckOperatorLogs(data []byte) ([]dbaas.OutuputMsg, error) {
	msgs := []dbaas.OutuputMsg{}

	lines := bytes.Split(data, []byte("\n"))
	for _, l := range lines {
		if len(l) == 0 {
			continue
		}

		entry := &operatorLog{}
		err := json.Unmarshal(l, entry)
		if err != nil {
			return nil, errors.Wrap(err, "unmarshal entry")
		}

		if entry.Controller != "perconaxtradbcluster-controller" {
			continue
		}

		// skips old entries
		if entry.TS <= p.opLogsLastTS {
			continue
		}

		p.opLogsLastTS = entry.TS

		if entry.Level != "error" {
			continue
		}

		cluster := ""
		s := strings.Split(entry.Request, "/")
		if len(s) == 2 {
			cluster = s[1]
		}

		if cluster != p.name {
			continue
		}

		msgs = append(msgs, alterOpError(entry))
	}

	return msgs, nil
}

func alterOpError(l *operatorLog) dbaas.OutuputMsg {
	if strings.Contains(l.Error, "the object has been modified; please apply your changes to the latest version and try again") {
		if i := strings.Index(l.Error, "Operation cannot be fulfilled on"); i >= 0 {
			return dbaas.OutuputMsgDebug(l.Error[i:])
		}
	}

	return dbaas.OutuputMsgError(l.Msg + ": " + l.Error)
}

func alterStatusMgs(msgs []string) []string {
	for i, msg := range msgs {
		msgs[i] = alterMessage(msg)
	}

	return msgs
}

func alterMessage(msg string) string {
	app := ""
	if i := strings.Index(msg, ":"); i >= 0 {
		app = msg[:i]
	}

	if strings.Contains(msg, "node(s) didn't match pod affinity/anti-affinity") {
		key := ""
		switch app {
		case "PXC":
			key = "--pxc-anti-affinity-key"
		case "ProxySQL":
			key = "--proxy-anti-affinity-key"
		}
		return fmt.Sprintf("Cluster node(s) didn't satisfy %s pods [anti-]affinity rules. Try to change %s parameter or add more nodes/change topology of your cluster.", app, key)
	}

	if strings.Contains(msg, "Insufficient memory.") {
		key := ""
		switch app {
		case "PXC":
			key = "--pxc-request-mem"
		case "ProxySQL":
			key = "--proxy-request-mem"
		}
		return fmt.Sprintf("Avaliable memory not enough to satisfy %s request. Try to change %s parameter or add more memmory to your cluster.", app, key)
	}

	if strings.Contains(msg, "Insufficient cpu.") {
		key := ""
		switch app {
		case "PXC":
			key = "--pxc-request-cpu"
		case "ProxySQL":
			key = "--proxy-request-cpu"
		}
		return fmt.Sprintf("Avaliable CPU not enough to satisfy %s request. Try to change %s parameter or add more CPU to your cluster.", app, key)
	}

	return msg
}