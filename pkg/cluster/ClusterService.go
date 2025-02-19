/*
 * Copyright (c) 2020 Devtron Labs
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package cluster

import (
	"context"
	"fmt"
	casbin2 "github.com/devtron-labs/devtron/pkg/user/casbin"
	repository2 "github.com/devtron-labs/devtron/pkg/user/repository"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"net/http"
	"net/url"
	"os"
	"time"

	bean2 "github.com/devtron-labs/devtron/api/bean"
	"github.com/devtron-labs/devtron/client/k8s/informer"
	"github.com/devtron-labs/devtron/internal/constants"
	"github.com/devtron-labs/devtron/internal/util"
	"github.com/devtron-labs/devtron/pkg/cluster/repository"
	util2 "github.com/devtron-labs/devtron/util"
	"github.com/go-pg/pg"
	"go.uber.org/zap"
)

const (
	DEFAULT_CLUSTER                  = "default_cluster"
	DEFAULT_NAMESPACE                = "default"
	CLUSTER_MODIFY_EVENT_SECRET_TYPE = "cluster.request/modify"
	CLUSTER_ACTION_ADD               = "add"
	CLUSTER_ACTION_UPDATE            = "update"
	SECRET_NAME                      = "cluster-event"
	SECRET_FIELD_CLUSTER_ID          = "cluster_id"
	SECRET_FIELD_UPDATED_ON          = "updated_on"
	SECRET_FIELD_ACTION              = "action"
)

type ClusterBean struct {
	Id                      int                        `json:"id,omitempty" validate:"number"`
	ClusterName             string                     `json:"cluster_name,omitempty" validate:"required"`
	ServerUrl               string                     `json:"server_url,omitempty" validate:"url,required"`
	PrometheusUrl           string                     `json:"prometheus_url,omitempty" validate:"validate-non-empty-url"`
	Active                  bool                       `json:"active"`
	Config                  map[string]string          `json:"config,omitempty"`
	PrometheusAuth          *PrometheusAuth            `json:"prometheusAuth,omitempty"`
	DefaultClusterComponent []*DefaultClusterComponent `json:"defaultClusterComponent"`
	AgentInstallationStage  int                        `json:"agentInstallationStage,notnull"` // -1=external, 0=not triggered, 1=progressing, 2=success, 3=fails
	K8sVersion              string                     `json:"k8sVersion"`
	HasConfigOrUrlChanged   bool                       `json:"-"`
	ErrorInConnecting       string                     `json:"errorInConnecting,omitempty"`
	IsCdArgoSetup           bool                       `json:"isCdArgoSetup"`
}

type PrometheusAuth struct {
	UserName      string `json:"userName,omitempty"`
	Password      string `json:"password,omitempty"`
	TlsClientCert string `json:"tlsClientCert,omitempty"`
	TlsClientKey  string `json:"tlsClientKey,omitempty"`
}

type DefaultClusterComponent struct {
	ComponentName  string `json:"name"`
	AppId          int    `json:"appId"`
	InstalledAppId int    `json:"installedAppId,omitempty"`
	EnvId          int    `json:"envId"`
	EnvName        string `json:"envName"`
	Status         string `json:"status"`
}

type ClusterService interface {
	Save(parent context.Context, bean *ClusterBean, userId int32) (*ClusterBean, error)
	FindOne(clusterName string) (*ClusterBean, error)
	FindOneActive(clusterName string) (*ClusterBean, error)
	FindAll() ([]*ClusterBean, error)
	FindAllWithoutConfig() ([]*ClusterBean, error)
	FindAllActive() ([]ClusterBean, error)
	DeleteFromDb(bean *ClusterBean, userId int32) error

	FindById(id int) (*ClusterBean, error)
	FindByIdWithoutConfig(id int) (*ClusterBean, error)
	FindByIds(id []int) ([]ClusterBean, error)
	Update(ctx context.Context, bean *ClusterBean, userId int32) (*ClusterBean, error)
	Delete(bean *ClusterBean, userId int32) error

	FindAllForAutoComplete() ([]ClusterBean, error)
	CreateGrafanaDataSource(clusterBean *ClusterBean, env *repository.Environment) (int, error)
	GetClusterConfig(cluster *ClusterBean) (*util.ClusterConfig, error)
	GetK8sClient() (*v12.CoreV1Client, error)
	GetAllClusterNamespaces() map[string][]string
	FindAllNamespacesByUserIdAndClusterId(userId int32, clusterId int, isActionUserSuperAdmin bool) ([]string, error)
	FindAllForClusterByUserId(userId int32, isActionUserSuperAdmin bool) ([]ClusterBean, error)
	FetchRolesFromGroup(userId int32) ([]*repository2.RoleModel, error)
}

type ClusterServiceImpl struct {
	clusterRepository   repository.ClusterRepository
	logger              *zap.SugaredLogger
	K8sUtil             *util.K8sUtil
	K8sInformerFactory  informer.K8sInformerFactory
	userAuthRepository  repository2.UserAuthRepository
	userRepository      repository2.UserRepository
	roleGroupRepository repository2.RoleGroupRepository
}

func NewClusterServiceImpl(repository repository.ClusterRepository, logger *zap.SugaredLogger,
	K8sUtil *util.K8sUtil, K8sInformerFactory informer.K8sInformerFactory,
	userAuthRepository repository2.UserAuthRepository, userRepository repository2.UserRepository,
	roleGroupRepository repository2.RoleGroupRepository) *ClusterServiceImpl {
	clusterService := &ClusterServiceImpl{
		clusterRepository:   repository,
		logger:              logger,
		K8sUtil:             K8sUtil,
		K8sInformerFactory:  K8sInformerFactory,
		userAuthRepository:  userAuthRepository,
		userRepository:      userRepository,
		roleGroupRepository: roleGroupRepository,
	}
	go clusterService.buildInformer()
	return clusterService
}

const DefaultClusterName = "default_cluster"
const TokenFilePath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

func (impl *ClusterServiceImpl) GetK8sClient() (*v12.CoreV1Client, error) {
	return impl.K8sUtil.GetK8sClient()
}

func (impl *ClusterServiceImpl) GetClusterConfig(cluster *ClusterBean) (*util.ClusterConfig, error) {
	host := cluster.ServerUrl
	configMap := cluster.Config
	bearerToken := configMap["bearer_token"]
	if cluster.Id == 1 && cluster.ClusterName == DefaultClusterName {
		if _, err := os.Stat(TokenFilePath); os.IsNotExist(err) {
			impl.logger.Errorw("no directory or file exists", "TOKEN_FILE_PATH", TokenFilePath, "err", err)
			return nil, err
		} else {
			content, err := ioutil.ReadFile(TokenFilePath)
			if err != nil {
				impl.logger.Errorw("error on reading file", "err", err)
				return nil, err
			}
			bearerToken = string(content)
		}
	}
	clusterCfg := &util.ClusterConfig{Host: host, BearerToken: bearerToken}
	return clusterCfg, nil
}

func (impl *ClusterServiceImpl) Save(parent context.Context, bean *ClusterBean, userId int32) (*ClusterBean, error) {
	//validating config
	err := impl.CheckIfConfigIsValid(bean)
	if err != nil {
		return nil, err
	}
	existingModel, err := impl.clusterRepository.FindOne(bean.ClusterName)
	if err != nil && err != pg.ErrNoRows {
		impl.logger.Error(err)
		return nil, err
	}
	if existingModel.Id > 0 {
		impl.logger.Errorw("error on fetching cluster, duplicate", "name", bean.ClusterName)
		return nil, fmt.Errorf("cluster already exists")
	}

	model := &repository.Cluster{
		ClusterName:        bean.ClusterName,
		Active:             bean.Active,
		ServerUrl:          bean.ServerUrl,
		Config:             bean.Config,
		PrometheusEndpoint: bean.PrometheusUrl,
	}

	if bean.PrometheusAuth != nil {
		model.PUserName = bean.PrometheusAuth.UserName
		model.PPassword = bean.PrometheusAuth.Password
		model.PTlsClientCert = bean.PrometheusAuth.TlsClientCert
		model.PTlsClientKey = bean.PrometheusAuth.TlsClientKey
	}

	model.CreatedBy = userId
	model.UpdatedBy = userId
	model.CreatedOn = time.Now()
	model.UpdatedOn = time.Now()

	cfg, err := impl.GetClusterConfig(bean)
	if err != nil {
		return nil, err
	}
	client, err := impl.K8sUtil.GetK8sDiscoveryClient(cfg)
	if err != nil {
		return nil, err
	}
	k8sServerVersion, err := client.ServerVersion()
	if err != nil {
		return nil, err
	}
	model.K8sVersion = k8sServerVersion.String()
	err = impl.clusterRepository.Save(model)
	if err != nil {
		impl.logger.Errorw("error in saving cluster in db", "err", err)
		err = &util.ApiError{
			Code:            constants.ClusterCreateDBFailed,
			InternalMessage: "cluster creation failed in db",
			UserMessage:     fmt.Sprintf("requested by %d", userId),
		}
	}
	bean.Id = model.Id

	//on successful creation of new cluster, update informer cache for namespace group by cluster
	//here sync for ea mode only
	if util2.IsBaseStack() {
		impl.SyncNsInformer(bean)
	}
	impl.logger.Info("saving secret for cluster informer")
	restConfig := &rest.Config{}
	restConfig, err = rest.InClusterConfig()
	if err != nil {
		impl.logger.Error("Error in creating config for default cluster", "err", err)
		return bean, nil
	}
	httpClientFor, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		impl.logger.Error("error occurred while overriding k8s client", "reason", err)
		return bean, nil
	}
	k8sClient, err := v12.NewForConfigAndClient(restConfig, httpClientFor)
	if err != nil {
		impl.logger.Error("error creating k8s client", "error", err)
		return bean, nil
	}
	//creating cluster secret, this secret will be read informer in kubelink to know that a new cluster has been added
	secretName := fmt.Sprintf("%s-%v", SECRET_NAME, bean.Id)

	data := make(map[string][]byte)
	data[SECRET_FIELD_CLUSTER_ID] = []byte(fmt.Sprintf("%v", bean.Id))
	data[SECRET_FIELD_ACTION] = []byte(CLUSTER_ACTION_ADD)
	data[SECRET_FIELD_UPDATED_ON] = []byte(time.Now().String()) // this field will ensure that informer detects change as other fields can be constant even if cluster config changes
	_, err = impl.K8sUtil.CreateSecret(DEFAULT_NAMESPACE, data, secretName, CLUSTER_MODIFY_EVENT_SECRET_TYPE, k8sClient)
	if err != nil {
		impl.logger.Errorw("error in updating secret for informers")
		return bean, nil
	}

	return bean, err
}

func (impl *ClusterServiceImpl) FindOne(clusterName string) (*ClusterBean, error) {
	model, err := impl.clusterRepository.FindOne(clusterName)
	if err != nil {
		return nil, err
	}
	bean := &ClusterBean{
		Id:                     model.Id,
		ClusterName:            model.ClusterName,
		ServerUrl:              model.ServerUrl,
		PrometheusUrl:          model.PrometheusEndpoint,
		AgentInstallationStage: model.AgentInstallationStage,
		Active:                 model.Active,
		Config:                 model.Config,
		K8sVersion:             model.K8sVersion,
	}
	return bean, nil
}

func (impl *ClusterServiceImpl) FindOneActive(clusterName string) (*ClusterBean, error) {
	model, err := impl.clusterRepository.FindOneActive(clusterName)
	if err != nil {
		return nil, err
	}
	bean := &ClusterBean{
		Id:                     model.Id,
		ClusterName:            model.ClusterName,
		ServerUrl:              model.ServerUrl,
		PrometheusUrl:          model.PrometheusEndpoint,
		AgentInstallationStage: model.AgentInstallationStage,
		Active:                 model.Active,
		Config:                 model.Config,
		K8sVersion:             model.K8sVersion,
	}
	return bean, nil
}

func (impl *ClusterServiceImpl) FindAllWithoutConfig() ([]*ClusterBean, error) {
	models, err := impl.FindAll()
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		model.Config = map[string]string{"bearer_token": ""}
	}
	return models, nil
}

func (impl *ClusterServiceImpl) FindAll() ([]*ClusterBean, error) {
	model, err := impl.clusterRepository.FindAllActive()
	if err != nil {
		return nil, err
	}
	var beans []*ClusterBean
	for _, m := range model {
		beans = append(beans, &ClusterBean{
			Id:                     m.Id,
			ClusterName:            m.ClusterName,
			PrometheusUrl:          m.PrometheusEndpoint,
			AgentInstallationStage: m.AgentInstallationStage,
			ServerUrl:              m.ServerUrl,
			Active:                 m.Active,
			K8sVersion:             m.K8sVersion,
			ErrorInConnecting:      m.ErrorInConnecting,
			Config:                 m.Config,
		})
	}
	return beans, nil
}

func (impl *ClusterServiceImpl) FindAllActive() ([]ClusterBean, error) {
	model, err := impl.clusterRepository.FindAllActive()
	if err != nil {
		return nil, err
	}
	var beans []ClusterBean
	for _, m := range model {
		beans = append(beans, ClusterBean{
			Id:                     m.Id,
			ClusterName:            m.ClusterName,
			ServerUrl:              m.ServerUrl,
			Active:                 m.Active,
			PrometheusUrl:          m.PrometheusEndpoint,
			AgentInstallationStage: m.AgentInstallationStage,
			Config:                 m.Config,
			K8sVersion:             m.K8sVersion,
			ErrorInConnecting:      m.ErrorInConnecting,
		})
	}
	return beans, nil
}

func (impl *ClusterServiceImpl) FindById(id int) (*ClusterBean, error) {
	model, err := impl.clusterRepository.FindById(id)
	if err != nil {
		return nil, err
	}
	bean := &ClusterBean{
		Id:                     model.Id,
		ClusterName:            model.ClusterName,
		ServerUrl:              model.ServerUrl,
		PrometheusUrl:          model.PrometheusEndpoint,
		AgentInstallationStage: model.AgentInstallationStage,
		Active:                 model.Active,
		Config:                 model.Config,
		K8sVersion:             model.K8sVersion,
	}
	prometheusAuth := &PrometheusAuth{
		UserName:      model.PUserName,
		Password:      model.PPassword,
		TlsClientCert: model.PTlsClientCert,
		TlsClientKey:  model.PTlsClientKey,
	}
	bean.PrometheusAuth = prometheusAuth
	return bean, nil
}

func (impl *ClusterServiceImpl) FindByIdWithoutConfig(id int) (*ClusterBean, error) {
	model, err := impl.FindById(id)
	if err != nil {
		return nil, err
	}
	//empty bearer token as it will be hidden for user
	model.Config = map[string]string{"bearer_token": ""}
	return model, nil
}

func (impl *ClusterServiceImpl) FindByIds(ids []int) ([]ClusterBean, error) {
	models, err := impl.clusterRepository.FindByIds(ids)
	if err != nil {
		return nil, err
	}
	var beans []ClusterBean

	for _, model := range models {
		beans = append(beans, ClusterBean{
			Id:                     model.Id,
			ClusterName:            model.ClusterName,
			ServerUrl:              model.ServerUrl,
			PrometheusUrl:          model.PrometheusEndpoint,
			AgentInstallationStage: model.AgentInstallationStage,
			Active:                 model.Active,
			Config:                 model.Config,
			K8sVersion:             model.K8sVersion,
		})
	}
	return beans, nil
}

func (impl *ClusterServiceImpl) Update(ctx context.Context, bean *ClusterBean, userId int32) (*ClusterBean, error) {
	model, err := impl.clusterRepository.FindById(bean.Id)
	if err != nil {
		impl.logger.Error(err)
		return nil, err
	}
	existingModel, err := impl.clusterRepository.FindOne(bean.ClusterName)
	if err != nil && err != pg.ErrNoRows {
		impl.logger.Error(err)
		return nil, err
	}
	if existingModel.Id > 0 && model.Id != existingModel.Id {
		impl.logger.Errorw("error on fetching cluster, duplicate", "name", bean.ClusterName)
		return nil, fmt.Errorf("cluster already exists")
	}

	// check whether config modified or not, if yes create informer with updated config
	dbConfig := model.Config["bearer_token"]
	requestConfig := bean.Config["bearer_token"]
	if len(requestConfig) == 0 {
		bean.Config = model.Config
	}
	if bean.ServerUrl != model.ServerUrl || dbConfig != requestConfig {
		bean.HasConfigOrUrlChanged = true
		//validating config
		err := impl.CheckIfConfigIsValid(bean)
		if err != nil {
			return nil, err
		}
	}
	model.ClusterName = bean.ClusterName
	model.ServerUrl = bean.ServerUrl
	model.PrometheusEndpoint = bean.PrometheusUrl

	if bean.PrometheusAuth != nil {
		if bean.PrometheusAuth.UserName != "" {
			model.PUserName = bean.PrometheusAuth.UserName
		}
		if bean.PrometheusAuth.Password != "" {
			model.PPassword = bean.PrometheusAuth.Password
		}
		if bean.PrometheusAuth.TlsClientCert != "" {
			model.PTlsClientCert = bean.PrometheusAuth.TlsClientCert
		}
		if bean.PrometheusAuth.TlsClientKey != "" {
			model.PTlsClientKey = bean.PrometheusAuth.TlsClientKey
		}
	}
	model.ErrorInConnecting = "" //setting empty because config to be updated is already validated
	model.Active = bean.Active
	model.Config = bean.Config
	model.UpdatedBy = userId
	model.UpdatedOn = time.Now()

	if model.K8sVersion == "" {
		cfg, err := impl.GetClusterConfig(bean)
		if err != nil {
			return nil, err
		}
		client, err := impl.K8sUtil.GetK8sDiscoveryClient(cfg)
		if err != nil {
			return nil, err
		}
		k8sServerVersion, err := client.ServerVersion()
		if err != nil {
			return nil, err
		}
		model.K8sVersion = k8sServerVersion.String()
	}
	err = impl.clusterRepository.Update(model)
	if err != nil {
		err = &util.ApiError{
			Code:            constants.ClusterUpdateDBFailed,
			InternalMessage: "cluster update failed in db",
			UserMessage:     fmt.Sprintf("requested by %d", userId),
		}
		return bean, err
	}
	bean.Id = model.Id

	//here sync for ea mode only
	if bean.HasConfigOrUrlChanged && util2.IsBaseStack() {
		impl.SyncNsInformer(bean)
	}
	impl.logger.Infow("saving secret for cluster informer")
	restConfig := &rest.Config{}

	restConfig, err = rest.InClusterConfig()
	if err != nil {
		impl.logger.Errorw("Error in creating config for default cluster", "err", err)
		return bean, nil
	}

	httpClientFor, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		impl.logger.Infow("error occurred while overriding k8s client", "reason", err)
		return bean, nil
	}
	k8sClient, err := v12.NewForConfigAndClient(restConfig, httpClientFor)
	if err != nil {
		impl.logger.Errorw("error creating k8s client", "error", err)
		return bean, nil
	}
	// below secret will act as an event for informer running on secret object in kubelink
	if bean.HasConfigOrUrlChanged {
		secretName := fmt.Sprintf("%s-%v", SECRET_NAME, bean.Id)
		secret, err := impl.K8sUtil.GetSecret(DEFAULT_NAMESPACE, secretName, k8sClient)
		statusError, _ := err.(*errors.StatusError)
		if err != nil && statusError.Status().Code != http.StatusNotFound {
			impl.logger.Errorw("secret not found", "err", err)
			return bean, nil
		}
		data := make(map[string][]byte)
		data[SECRET_FIELD_CLUSTER_ID] = []byte(fmt.Sprintf("%v", bean.Id))
		data[SECRET_FIELD_ACTION] = []byte(CLUSTER_ACTION_UPDATE)
		data[SECRET_FIELD_UPDATED_ON] = []byte(time.Now().String()) // this field will ensure that informer detects change as other fields can be constant even if cluster config changes
		if secret == nil {
			_, err = impl.K8sUtil.CreateSecret(DEFAULT_NAMESPACE, data, secretName, CLUSTER_MODIFY_EVENT_SECRET_TYPE, k8sClient)
			if err != nil {
				impl.logger.Errorw("error in creating secret for informers")
			}
		} else {
			secret.Data = data
			secret, err = impl.K8sUtil.UpdateSecret(DEFAULT_NAMESPACE, secret, k8sClient)
			if err != nil {
				impl.logger.Errorw("error in updating secret for informers")
			}
		}
	}
	return bean, nil
}

func (impl *ClusterServiceImpl) SyncNsInformer(bean *ClusterBean) {
	requestConfig := bean.Config["bearer_token"]
	//before creating new informer for cluster, close existing one
	impl.K8sInformerFactory.CleanNamespaceInformer(bean.ClusterName)
	//create new informer for cluster with new config
	clusterInfo := &bean2.ClusterInfo{
		ClusterId:   bean.Id,
		ClusterName: bean.ClusterName,
		BearerToken: requestConfig,
		ServerUrl:   bean.ServerUrl,
	}
	impl.K8sInformerFactory.BuildInformer([]*bean2.ClusterInfo{clusterInfo})
}

func (impl *ClusterServiceImpl) Delete(bean *ClusterBean, userId int32) error {
	model, err := impl.clusterRepository.FindById(bean.Id)
	if err != nil {
		return err
	}
	return impl.clusterRepository.Delete(model)
}

func (impl *ClusterServiceImpl) FindAllForAutoComplete() ([]ClusterBean, error) {
	model, err := impl.clusterRepository.FindAll()
	if err != nil {
		return nil, err
	}
	var beans []ClusterBean
	for _, m := range model {
		beans = append(beans, ClusterBean{
			Id:                m.Id,
			ClusterName:       m.ClusterName,
			ErrorInConnecting: m.ErrorInConnecting,
			IsCdArgoSetup:     m.CdArgoSetup,
		})
	}
	return beans, nil
}

func (impl *ClusterServiceImpl) CreateGrafanaDataSource(clusterBean *ClusterBean, env *repository.Environment) (int, error) {
	impl.logger.Errorw("CreateGrafanaDataSource not inplementd in ClusterServiceImpl")
	return 0, fmt.Errorf("method not implemented")
}

func (impl *ClusterServiceImpl) buildInformer() {
	models, err := impl.clusterRepository.FindAllActive()
	if err != nil {
		impl.logger.Errorw("error in fetching clusters", "err", err)
		return
	}
	var clusterInfo []*bean2.ClusterInfo
	for _, model := range models {
		bearerToken := model.Config["bearer_token"]
		clusterInfo = append(clusterInfo, &bean2.ClusterInfo{
			ClusterId:   model.Id,
			ClusterName: model.ClusterName,
			BearerToken: bearerToken,
			ServerUrl:   model.ServerUrl,
		})
	}
	impl.K8sInformerFactory.BuildInformer(clusterInfo)
}

func (impl ClusterServiceImpl) DeleteFromDb(bean *ClusterBean, userId int32) error {
	existingCluster, err := impl.clusterRepository.FindById(bean.Id)
	if err != nil {
		impl.logger.Errorw("No matching entry found for delete.", "id", bean.Id)
		return err
	}
	deleteReq := existingCluster
	deleteReq.UpdatedOn = time.Now()
	deleteReq.UpdatedBy = userId
	err = impl.clusterRepository.MarkClusterDeleted(deleteReq)
	if err != nil {
		impl.logger.Errorw("error in deleting cluster", "id", bean.Id, "err", err)
		return err
	}
	restConfig := &rest.Config{}
	restConfig, err = rest.InClusterConfig()

	if err != nil {
		impl.logger.Errorw("Error in creating config for default cluster", "err", err)
		return nil
	}
	// this secret was created for syncing cluster information with kubelink
	httpClientFor, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		impl.logger.Errorw("error occurred while overriding k8s client", "reason", err)
		return nil
	}
	k8sClient, err := v12.NewForConfigAndClient(restConfig, httpClientFor)
	if err != nil {
		impl.logger.Errorw("error creating k8s client", "error", err)
		return nil
	}
	secretName := fmt.Sprintf("%s-%v", SECRET_NAME, bean.Id)
	err = impl.K8sUtil.DeleteSecret(DEFAULT_NAMESPACE, secretName, k8sClient)
	impl.logger.Errorw("error in deleting secret", "error", err)
	return nil
}

func (impl ClusterServiceImpl) CheckIfConfigIsValid(cluster *ClusterBean) error {
	configMap := cluster.Config
	bearerToken := configMap["bearer_token"]
	var restConfig *rest.Config
	var err error
	if cluster.ClusterName == DEFAULT_CLUSTER && len(bearerToken) == 0 {
		restConfig, err = rest.InClusterConfig()
		if err != nil {
			impl.logger.Errorw("error in getting rest config for default cluster", "err", err)
			return err
		}
	} else {
		restConfig = &rest.Config{Host: cluster.ServerUrl, BearerToken: bearerToken, TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	}
	k8sHttpClient, err := util.OverrideK8sHttpClientWithTracer(restConfig)
	if err != nil {
		return err
	}
	k8sClientSet, err := kubernetes.NewForConfigAndClient(restConfig, k8sHttpClient)
	if err != nil {
		impl.logger.Errorw("error in getting client set by rest config", "err", err, "restConfig", restConfig)
		return err
	}
	//using livez path as healthz path is deprecated
	path := "/livez"
	response, err := k8sClientSet.Discovery().RESTClient().Get().AbsPath(path).DoRaw(context.Background())
	if err != nil {
		if _, ok := err.(*url.Error); ok {
			return fmt.Errorf("Incorrect server url : %v", err)
		} else if statusError, ok := err.(*errors.StatusError); ok {
			if statusError != nil {
				errReason := statusError.ErrStatus.Reason
				var errMsg string
				if errReason == v1.StatusReasonUnauthorized {
					errMsg = "token seems invalid or does not have sufficient permissions"
				} else {
					errMsg = statusError.ErrStatus.Message
				}
				return fmt.Errorf("%s : %s", errReason, errMsg)
			} else {
				return fmt.Errorf("Validation failed : %v", err)
			}
		} else {
			return fmt.Errorf("Validation failed : %v", err)
		}
	} else if err == nil && string(response) != "ok" {
		return fmt.Errorf("Validation failed with response : %s", string(response))
	}
	return nil
}

func (impl *ClusterServiceImpl) GetAllClusterNamespaces() map[string][]string {
	result := make(map[string][]string)
	namespaceListGroupByCLuster := impl.K8sInformerFactory.GetLatestNamespaceListGroupByCLuster()
	for clusterName, namespaces := range namespaceListGroupByCLuster {
		copiedNamespaces := result[clusterName]
		for namespace, value := range namespaces {
			if value {
				copiedNamespaces = append(copiedNamespaces, namespace)
			}
		}
		result[clusterName] = copiedNamespaces
	}
	return result
}

func (impl *ClusterServiceImpl) FindAllNamespacesByUserIdAndClusterId(userId int32, clusterId int, isActionUserSuperAdmin bool) ([]string, error) {
	result := make([]string, 0)
	clusterBean, err := impl.FindById(clusterId)
	if err != nil {
		impl.logger.Errorw("failed to find cluster for id", "error", err, "clusterId", clusterId)
		return nil, err
	}
	namespaceListGroupByCLuster := impl.K8sInformerFactory.GetLatestNamespaceListGroupByCLuster()
	namespaces := namespaceListGroupByCLuster[clusterBean.ClusterName]

	if isActionUserSuperAdmin {
		for namespace, value := range namespaces {
			if value {
				result = append(result, namespace)
			}
		}
	} else {
		roles, err := impl.FetchRolesFromGroup(userId)
		if err != nil {
			impl.logger.Errorw("error on fetching user roles for cluster list", "err", err)
			return nil, err
		}
		allowedAll := false
		allowedNamespaceMap := make(map[string]bool)
		for _, role := range roles {
			if clusterBean.ClusterName == role.Cluster {
				allowedNamespaceMap[role.Namespace] = true
				if role.Namespace == "" {
					allowedAll = true
				}
			}
		}

		//adding final namespace list
		for namespace, value := range namespaces {
			if _, ok := allowedNamespaceMap[namespace]; ok || allowedAll {
				if value {
					result = append(result, namespace)
				}
			}
		}
	}
	return result, nil
}

func (impl *ClusterServiceImpl) FindAllForClusterByUserId(userId int32, isActionUserSuperAdmin bool) ([]ClusterBean, error) {
	if isActionUserSuperAdmin {
		return impl.FindAllForAutoComplete()
	}
	allowedClustersMap := make(map[string]bool)
	roles, err := impl.FetchRolesFromGroup(userId)
	if err != nil {
		impl.logger.Errorw("error while fetching user roles from db", "error", err)
		return nil, err
	}
	for _, role := range roles {
		allowedClustersMap[role.Cluster] = true
	}

	models, err := impl.clusterRepository.FindAll()
	if err != nil {
		impl.logger.Errorw("error on fetching clusters", "err", err)
		return nil, err
	}
	var beans []ClusterBean
	for _, model := range models {
		if _, ok := allowedClustersMap[model.ClusterName]; ok {
			beans = append(beans, ClusterBean{
				Id:                model.Id,
				ClusterName:       model.ClusterName,
				ErrorInConnecting: model.ErrorInConnecting,
			})
		}
	}
	return beans, nil
}

func (impl *ClusterServiceImpl) FetchRolesFromGroup(userId int32) ([]*repository2.RoleModel, error) {
	user, err := impl.userRepository.GetByIdIncludeDeleted(userId)
	if err != nil {
		impl.logger.Errorw("error while fetching user from db", "error", err)
		return nil, err
	}
	groups, err := casbin2.GetRolesForUser(user.EmailId)
	if err != nil {
		impl.logger.Errorw("No Roles Found for user", "id", user.Id)
		return nil, err
	}
	roleEntity := "cluster"
	roles, err := impl.userAuthRepository.GetRolesByUserIdAndEntityType(userId, roleEntity)
	if err != nil {
		impl.logger.Errorw("error on fetching user roles for cluster list", "err", err)
		return nil, err
	}
	rolesFromGroup, err := impl.roleGroupRepository.GetRolesByGroupNamesAndEntity(groups, roleEntity)
	if err != nil && err != pg.ErrNoRows {
		impl.logger.Errorw("error in getting roles by group names", "err", err)
		return nil, err
	}
	if len(rolesFromGroup) > 0 {
		roles = append(roles, rolesFromGroup...)
	}
	return roles, nil
}
