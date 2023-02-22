package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/gofrs/uuid"
	"github.com/gorilla/mux"
	"github.com/layer5io/meshery/server/meshes"
	"github.com/layer5io/meshery/server/models"
	"github.com/layer5io/meshery/server/models/pattern/core"
	"github.com/layer5io/meshery/server/models/pattern/patterns"
	"github.com/layer5io/meshery/server/models/pattern/patterns/k8s"
	"github.com/layer5io/meshery/server/models/pattern/stages"
	"github.com/layer5io/meshkit/logger"
	"github.com/layer5io/meshkit/models/meshmodel"
	"github.com/layer5io/meshkit/models/oam/core/v1alpha1"
	"github.com/layer5io/meshkit/utils/events"
	meshkube "github.com/layer5io/meshkit/utils/kubernetes"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/client-go/applyconfigurations/meta/v1"
)

// patternCallType is custom type for pattern
// based calls on the adapter
type patternCallType string

const (
	rawAdapter patternCallType = "<raw-adapter>"
	noneLocal  patternCallType = "<none-local>"
	oamAdapter patternCallType = ""
)

// swagger:route POST /api/pattern/deploy PatternsAPI idPostDeployPattern
// Handle POST request for Pattern Deploy
//
// Deploy an attached pattern with the request
// responses:
// 	200:

// swagger:route DELETE /api/pattern/deploy PatternsAPI idDeleteDeployPattern
// Handle DELETE request for Pattern Deploy
//
// Delete a deployed pattern with the request
// responses:
// 	200:

// PatternFileHandler handles the requested related to pattern files
func (h *Handler) PatternFileHandler(
	rw http.ResponseWriter,
	r *http.Request,
	prefObj *models.Preference,
	user *models.User,
	provider models.Provider,
) {
	// Read the PatternFile
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error(ErrRequestBody(err))
		http.Error(rw, ErrRequestBody(err).Error(), http.StatusInternalServerError)

		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, "failed to read request body: %s", err)
		return
	}

	if r.Header.Get("Content-Type") == "application/json" {
		body, err = yaml.JSONToYAML(body)
		if err != nil {
			h.log.Error(ErrPatternFile(err))
			http.Error(rw, ErrPatternFile(err).Error(), http.StatusInternalServerError)
			return
		}
	}

	isDel := r.Method == http.MethodDelete

	// Generate the pattern file object
	patternFile, err := core.NewPatternFile(body)
	if err != nil {
		h.log.Error(ErrPatternFile(err))
		http.Error(rw, ErrPatternFile(err).Error(), http.StatusInternalServerError)
		return
	}

	response, err := _processPattern(
		r.Context(),
		provider,
		patternFile,
		prefObj,
		user.UserID,
		isDel,
		r.URL.Query().Get("verify") == "true",
		r.URL.Query().Get("dryRun") == "true",
		false,
		h.registryManager,
		h.EventsBuffer,
		h.log,
	)
	if err != nil {
		h.log.Error(ErrCompConfigPairs(err))
		http.Error(rw, ErrCompConfigPairs(err).Error(), http.StatusInternalServerError)
		return
	}
	ec := json.NewEncoder(rw)
	_ = ec.Encode(response)
}

// swagger:route GET /api/oam/{type} PatternsAPI idGetOAMRegister
// Handles GET requests for list of OAM objects
//
// Returns a list of workloads/traits/scopes by given type in the URL
//
// {type} being of either trait, scope, workload; registration of adapter capabilities.
// Example: /api/oam/workload => Here {type} is "workload"
//
// deprecated: true
//
// responses:
// 	200:

// swagger:route POST /api/oam/{type} PatternsAPI idPostOAMRegister
// Handles POST requests for adding OAM objects
//
// Adding a workloads/traits/scopes by given type in the URL
//
// {type} being of either trait, scope, workload; registration of adapter capabilities.
// Example: /api/oam/workload => Here {type} is "workload"
//
// deprecated: true
//
// responses:
// 	200:

// OAMRegisterHandler handles OAM registry related operations
//
// These operations can be:
// 1. Adding a workload/trait/scope
// 2. Getting list of workloads/traits/scopes
func (h *Handler) OAMRegisterHandler(rw http.ResponseWriter, r *http.Request) {
	typ := mux.Vars(r)["type"]
	if !(typ == "workload" || typ == "trait" || typ == "scope") {
		rw.WriteHeader(http.StatusNotFound)
		return
	}
	method := r.Method
	if method == "POST" {
		if err := h.POSTOAMRegisterHandler(typ, r); err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
			h.log.Debug(err)
			_, _ = rw.Write([]byte(err.Error()))
			return
		}
	}
}

// swagger:route GET /api/oam/{type}/{name} PatternsAPI idOAMComponentDetails
// Handles GET requests for component details for OAM objects
//
// Returns component details of a workload/trait/scope by given name in the URL
//
// {type} being of either trait, scope, workload; registration of adapter capabilities.
// Example: /api/oam/workload/Application => Here {type} is "workload" and {name} is "Application"
// it should be noted that both {type} and {name} should be valid
//
// responses:
// 	200:

func (h *Handler) OAMComponentDetailsHandler(rw http.ResponseWriter, r *http.Request) {
	typ := mux.Vars(r)["type"]

	if !(typ == "workload" || typ == "trait" || typ == "scope") {
		rw.WriteHeader(http.StatusNotFound)
		return
	}

	name := mux.Vars(r)["name"]
	res := []interface{}{}

	if typ == "workload" {
		data := core.GetWorkload(name)
		for _, d := range data {
			res = append(res, d)
		}
	}

	if typ == "trait" {
		data := core.GetTrait(name)
		for _, d := range data {
			res = append(res, d)
		}
	}

	if typ == "scope" {
		data := core.GetScope(name)
		for _, d := range data {
			res = append(res, d)
		}
	}

	if err := json.NewEncoder(rw).Encode(res); err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		h.log.Debug(err)
		_, _ = rw.Write([]byte(err.Error()))
	}
}

// swagger:route GET /api/oam/{type}/{name}/{id} PatternsAPI idOAMComponentDetailByID
// Handles GET requests for component details for OAM objects
//
// Returns details of a workload/trait/scope by given name and id in the URL
//
// {type} being of either trait, scope, workload; registration of adapter capabilities.
// Example: /api/oam/workload/Application/asdqe123sa275sasd => Here {type} is "workload"
// {name} is "Application" and {id} is "asdqe123sa275sasd". It should be noted that all of three, i.e {type},
// {name} and {id} must be valid
//
// responses:
// 	200:

func (h *Handler) OAMComponentDetailByIDHandler(rw http.ResponseWriter, r *http.Request) {
	typ := mux.Vars(r)["type"]

	if !(typ == "workload" || typ == "trait" || typ == "scope") {
		rw.WriteHeader(http.StatusNotFound)
		return
	}

	name := mux.Vars(r)["name"]
	id := mux.Vars(r)["id"]
	var res interface{}

	if typ == "workload" {
		res = core.GetWorkloadByID(name, id)
	}

	if typ == "trait" {
		res = core.GetTraitByID(name, id)
	}

	if typ == "scope" {
		res = core.GetScopeByID(name, id)
	}

	if res == nil {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}

	if err := json.NewEncoder(rw).Encode(res); err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		h.log.Debug(err)
		_, _ = rw.Write([]byte(err.Error()))
	}
}

// swagger:route POST /api/oam/{type} PatternsAPI idPOSTOAMRegister
// Handles registering OMA objects
//
// Adding a workload/trait/scope
//
// {type} being of either trait, scope, workload; registration of adapter capabilities.
// Example: /api/oam/trait => Here {type} is "trait"
//
// responses:
// 	200:

// POSTOAMRegisterHandler handles registering OMA objects
func (h *Handler) POSTOAMRegisterHandler(typ string, r *http.Request) error {
	// Get the body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}

	if typ == "workload" {
		return core.RegisterWorkload(body)
	}
	if typ == "trait" {
		return core.RegisterTrait(body)
	}
	if typ == "scope" {
		return core.RegisterScope(body)
	}

	return nil
}

// swagger:route GET /api/oam/{type} PatternsAPI idGETOAMMesheryPattern
// Handles the get requests for the OAM objects
//
// Getting list of workloads/traits/scopes
//
// {type} being of either trait, scope, workload; registration of adapter capabilities.
// Example: /api/oam/workload => Here {type} is "workload"
//
// responses:
// 	200:

// GETOAMRegisterHandler handles the get requests for the OAM objects
func (h *Handler) GETOAMRegisterHandler(rw http.ResponseWriter,
	r *http.Request) {
	typ := mux.Vars(r)["type"]
	if !(typ == "workload" || typ == "trait" || typ == "scope") {
		rw.WriteHeader(http.StatusNotFound)
		return
	}
	trim := r.URL.Query().Get("trim") == "true"
	rw.Header().Add("Content-Type", "application/json")
	enc := json.NewEncoder(rw)

	if typ == "workload" {
		res := core.GetWorkloads()

		// If trim is set to true then remove the schema from the response
		if trim {
			for i := range res {
				res[i].OAMRefSchema = ""
			}
		}

		if err := enc.Encode(res); err != nil {
			h.log.Error(ErrWorkloadDefinition(err))
			http.Error(rw, ErrWorkloadDefinition(err).Error(), http.StatusInternalServerError)
		}
	}

	if typ == "trait" {
		res := core.GetTraits()

		// If trim is set to true then remove the schema from the response
		if trim {
			for i := range res {
				res[i].OAMRefSchema = ""
			}
		}

		enc := json.NewEncoder(rw)
		if err := enc.Encode(res); err != nil {
			h.log.Error(ErrTraitDefinition(err))
			http.Error(rw, ErrScopeDefinition(err).Error(), http.StatusInternalServerError)
		}
	}

	if typ == "scope" {
		res := core.GetScopes()

		// If trim is set to true then remove the schema from the response
		if trim {
			for i := range res {
				res[i].OAMRefSchema = ""
			}
		}

		enc := json.NewEncoder(rw)
		if err := enc.Encode(res); err != nil {
			h.log.Error(ErrScopeDefinition(err))
			http.Error(rw, ErrScopeDefinition(err).Error(), http.StatusInternalServerError)
		}
	}
}

func mergeMsgs(msgs []string) string {
	var finalMsgs []string

	for _, msg := range msgs {
		if msg != "" {
			finalMsgs = append(finalMsgs, msg)
		}
	}

	return strings.Join(finalMsgs, "\n")
}

func _processPattern(
	ctx context.Context,
	provider models.Provider,
	pattern core.Pattern,
	prefObj *models.Preference,
	userID string,
	isDelete bool,
	verify bool,
	dryRun bool,
	skipPrintLogs bool,
	registry *meshmodel.RegistryManager,
	eb *events.EventStreamer,
	l logger.Handler,
) (map[string]interface{}, error) {
	resp := make(map[string]interface{})

	// Get the token from the context
	token, ok := ctx.Value(models.TokenCtxKey).(string)
	if !ok {
		return nil, ErrRetrieveUserToken(fmt.Errorf("token not found in the context"))
	}
	// // Get the kubehandler from the context
	k8scontexts, ok := ctx.Value(models.KubeClustersKey).([]models.K8sContext)
	if !ok || len(k8scontexts) == 0 {
		return nil, ErrInvalidKubeHandler(fmt.Errorf("failed to find k8s handler"), "_processPattern couldn't find a valid k8s handler")
	}

	// // Get the kubernetes config from the context
	// kubecfg, ok := ctx.Value(models.KubeConfigKey).([]byte)
	// if !ok || kubecfg == nil {
	// 	return "", ErrInvalidKubeConfig(fmt.Errorf("failed to find k8s config"), "_processPattern couldn't find a valid k8s config")
	// }

	// // Get the kubernetes context from the context
	// mk8scontext, ok := ctx.Value(models.KubeContextKey).(*models.K8sContext)
	// if !ok || mk8scontext == nil {
	// 	return "", ErrInvalidKubeContext(fmt.Errorf("failed to find k8s context"), "_processPattern couldn't find a valid k8s context")
	// }
	var ctxToconfig = make(map[string]string)
	for _, ctx := range k8scontexts {
		cfg, err := ctx.GenerateKubeConfig()
		if err != nil {
			return nil, ErrInvalidKubeConfig(fmt.Errorf("failed to find k8s config"), "_processPattern couldn't find a valid k8s config")
		}
		ctxToconfig[ctx.ID] = string(cfg)
		// configs = append(configs, string(cfg))
	}
	internal := func(mk8scontext []models.K8sContext) (map[string]interface{}, error) {
		sip := &serviceInfoProvider{
			token:      token,
			provider:   provider,
			opIsDelete: isDelete,
		}
		sap := &serviceActionProvider{
			token:    token,
			log:      l,
			provider: provider,
			prefObj:  prefObj,
			// kubeClient:    kubeClient,
			opIsDelete: isDelete,
			userID:     userID,
			registry:   registry,
			// kubeconfig:    kubecfg,
			// kubecontext:   mk8scontext,
			skipPrintLogs:   skipPrintLogs,
			ctxTokubeconfig: ctxToconfig,
			accumulatedMsgs: []string{},
			err:             nil,
			eventbuffer:     eb,
		}
		chain := stages.CreateChain()
		chain.
			Add(stages.Import(sip, sap)).
			Add(stages.ServiceIdentifierAndMutator(sip, sap)).
			Add(stages.Filler(skipPrintLogs)).
			// Calling this stage `The Validation stage` is a bit deceiving considering
			// that the validation stage also formats the `data` (chain function parameter) that the
			// subsequent stages depend on.
			// We are skipping the `Validation` part in case of dryRun
			Add(stages.Validator(sip, sap, dryRun))
		if dryRun {
			chain.Add(stages.DryRun(sip, sap))
		}
		if !verify && !dryRun {
			chain.
				Add(stages.Provision(sip, sap)).
				Add(stages.Persist(sip, sap))
		}
		chain.
			Add(func(data *stages.Data, err error, next stages.ChainStageNextFunction) {
				data.Lock.Lock()
				for k, v := range data.Other {
					if strings.HasSuffix(k, stages.ProvisionSuffixKey) {
						msg, ok := v.(string)
						if ok {
							sap.accumulatedMsgs = append(sap.accumulatedMsgs, msg)
						}
					}
					if k == stages.DryRunResponseKey {
						if v != nil {
							resp["dryRunResponse"] = v
						}
					}
				}
				data.Lock.Unlock()
				sap.err = err
			}).
			Process(&stages.Data{
				Pattern: &pattern,
				Other:   map[string]interface{}{},
			})
		resp["messages"] = mergeMsgs(sap.accumulatedMsgs)
		return resp, sap.err
	}
	return internal(k8scontexts)
	// customK8scontexts, ok := ctx.Value(models.KubeClustersKey).([]models.K8sContext)
	// if ok && len(customK8scontexts) > 0 {
	// 	var wg sync.WaitGroup
	// 	resp := []string{}
	// 	errs := []string{}

	// 	for _, c := range customK8scontexts {
	// 		wg.Add(1)
	// 		go func(c *models.K8sContext) {
	// 			defer wg.Done()

	// 			// Generate Kube Handler
	// 			kh, err := c.GenerateKubeHandler()
	// 			if err != nil {
	// 				errs = append(errs, err.Error())
	// 				return
	// 			}

	// 			// Generate kube config
	// 			kcfg, err := c.GenerateKubeConfig()
	// 			if err != nil {
	// 				errs = append(errs, err.Error())
	// 				return
	// 			}

	// 			res, err := internal(k8scontexts)
	// 			if err != nil {
	// 				errs = append(errs, err.Error())
	// 				return
	// 			}

	// 			resp = append(resp, res)
	// 		}(&c)
	// 	}

	// 	wg.Wait()

	// 	if len(errs) == 0 {
	// 		return mergeMsgs(resp), nil
	// 	}

	// 	return mergeMsgs(resp), fmt.Errorf(mergeMsgs(errs))
	// }
}

type serviceInfoProvider struct {
	provider   models.Provider
	token      string
	opIsDelete bool
}

func (sip *serviceInfoProvider) GetMesheryPatternResource(name, namespace, typ, oamType string) (*uuid.UUID, error) {
	const page = "0"
	const pageSize = "1"
	res, err := sip.provider.GetMesheryPatternResources(sip.token, pageSize, page, "", "", name, namespace, typ, oamType)
	if err != nil {
		return nil, err
	}

	if len(res.Resources) > 0 {
		return res.Resources[0].ID, nil
	}

	return nil, fmt.Errorf("resource not found")
}

func (sip *serviceInfoProvider) GetServiceMesh() (string, string) {
	return "", ""
}

func (sip *serviceInfoProvider) GetAPIVersionForKind(string) string {
	return ""
}

func (sip *serviceInfoProvider) IsDelete() bool {
	return sip.opIsDelete
}

type serviceActionProvider struct {
	token    string
	log      logger.Handler
	provider models.Provider
	prefObj  *models.Preference
	// kubeClient      *meshkube.Client
	ctxTokubeconfig map[string]string
	opIsDelete      bool
	userID          string
	// kubeconfig  []byte
	// kubecontext     *models.K8sContext
	skipPrintLogs   bool
	accumulatedMsgs []string
	err             error
	eventbuffer     *events.EventStreamer
	registry        *meshmodel.RegistryManager
}

func (sap *serviceActionProvider) GetRegistry() *meshmodel.RegistryManager {
	return sap.registry
}

func (sap *serviceActionProvider) Log(msg string) {
	if sap.log != nil {
		sap.log.Info(msg)
	}
}
func (sap *serviceActionProvider) Terminate(err error) {
	if !sap.skipPrintLogs {
		logrus.Error(err)
	}
	sap.err = err
}
func (sap *serviceActionProvider) Mutate(p *core.Pattern) {
	//TODO: externalize these mutation rules with policies.
	//1. Enforce the deployment of CRDs before other resources
	for name, svc := range p.Services {
		if svc.Type == "CustomResourceDefinition.K8s" {
			for _, svc := range p.Services {
				if svc.Type != "CustomResourceDefinition.K8s" {
					svc.DependsOn = append(svc.DependsOn, name)
				}
			}
		}
	}
}

// NOTE: Currently tied to kubernetes
// Returns ComponentName->ContextID->Response
func (sap *serviceActionProvider) DryRun(comps []v1alpha1.Component) (resp map[string]map[string]core.DryRunResponse2, err error) {
	for _, cmp := range comps {
		for ctxID, kc := range sap.ctxTokubeconfig {
			cl, err := meshkube.New([]byte(kc))
			if err != nil {
				return resp, err
			}
			st, ok, err := k8s.DryRunHelper(cl, cmp)
			if err != nil {
				return resp, err
			}
			dResp := core.DryRunResponse2{Success: ok}
			if ok {
				dResp.Component = &core.Service{
					Name:        cmp.Name,
					Type:        cmp.Spec.Type,
					Namespace:   cmp.Namespace,
					APIVersion:  cmp.Spec.APIVersion,
					Version:     cmp.Spec.Version,
					Model:       cmp.Spec.Model,
					Labels:      cmp.Labels,
					Annotations: cmp.Annotations,
				}
				dResp.Component.Settings = make(map[string]interface{})
				for k, v := range st {
					if k == "apiVersion" || k == "kind" || k == "metadata" {
						continue
					}
					dResp.Component.Settings[k] = v
				}
				if resp == nil {
					resp = make(map[string]map[string]core.DryRunResponse2)
				}
				if resp[cmp.Name] == nil {
					resp[cmp.Name] = make(map[string]core.DryRunResponse2)
				}
				resp[cmp.Name][ctxID] = dResp
				lol, _ := json.MarshalIndent(resp, "", " ")
				fmt.Println("will send to client", string(lol))
				return resp, nil
			}
			dResp.Error = &core.DryRunResponse{}
			byt, err := json.Marshal(st)
			if err != nil {
				return nil, err
			}
			var a v1.StatusApplyConfiguration
			err = json.Unmarshal(byt, &a)
			if err != nil {
				return nil, err
			}
			if a.Status != nil {
				dResp.Error.Status = *a.Status
			}
			dResp.Error.Causes = make([]core.DryRunFailureCause, 0)
			fmt.Println((*a.APIVersion), *a.Kind)
			for _, c := range a.Details.Causes {
				msg := ""
				field := ""
				typ := ""
				if c.Message != nil {
					msg = *c.Message
				}
				if c.Field != nil {
					field = cmp.Name + "." + getComponentFieldPathFromK8sFieldPath(*c.Field)
				}
				if c.Type != nil {
					typ = string(*c.Type)
				}
				failureCase := core.DryRunFailureCause{Message: msg, FieldPath: field, Type: typ}
				dResp.Error.Causes = append(dResp.Error.Causes, failureCase)
			}
			if resp == nil {
				resp = make(map[string]map[string]core.DryRunResponse2)
			}
			if resp[cmp.Name] == nil {
				resp[cmp.Name] = make(map[string]core.DryRunResponse2)
			}
			resp[cmp.Name][ctxID] = dResp
			lol, _ := json.MarshalIndent(resp, "", " ")
			fmt.Println("will send to client", string(lol))
			return resp, nil
		}
	}
	return
}
func getComponentFieldPathFromK8sFieldPath(path string) (newpath string) {
	if strings.HasPrefix(path, "metadata.") {
		path = strings.TrimPrefix(path, "metadata.")
		paths := strings.Split(path, ".")
		if len(paths) != 0 {
			if paths[0] == "name" || paths[0] == "namespace" || paths[0] == "labels" || paths[0] == "annotations" {
				return paths[0]
			}
		}
		return
	}
	return fmt.Sprintf("%s.%s", "settings", path)
}
func (sap *serviceActionProvider) Provision(ccp stages.CompConfigPair) (string, error) { // Marshal the component
	jsonComp, err := json.Marshal(ccp.Component)
	if err != nil {
		return "", fmt.Errorf("failed to serialize the data: %s", err)
	}

	// Marshal the configuration
	jsonConfig, err := json.Marshal(ccp.Configuration)
	if err != nil {
		return "", fmt.Errorf("failed to serialize the data: %s", err)
	}

	for host := range ccp.Hosts {
		// Hack until adapters fix the concurrent client
		// creation issue: https://github.com/layer5io/meshery-adapter-library/issues/32
		time.Sleep(50 * time.Microsecond)

		logrus.Debugf("Adapter to execute operations on: %s", host.Hostname)

		// Local call
		if host.Port == 0 {
			//TODO: Accomodate internal calls to use context mapping with kubeconfig
			var kconfigs []string
			for _, v := range sap.ctxTokubeconfig {
				kconfigs = append(kconfigs, v)
			}
			resp, err := patterns.ProcessOAM(
				kconfigs,
				[]string{string(jsonComp)},
				string(jsonConfig),
				sap.opIsDelete,
				sap.eventbuffer,
			)
			return resp, err
		}
		addr := host.Hostname
		if host.Port != 0 {
			addr += ":" + strconv.Itoa(host.Port)
		}
		// Create mesh client
		mClient, err := meshes.CreateClient(
			context.TODO(),
			addr,
		)
		if err != nil {
			return "", fmt.Errorf("error creating a mesh client: %v", err)
		}
		defer func() {
			_ = mClient.Close()
		}()

		// Execute operation on the adapter with raw data
		// if strings.HasPrefix(adapter, string(rawAdapter)) {
		// 	resp, err := mClient.MClient.ApplyOperation(context.TODO(), &meshes.ApplyRuleRequest{
		// 		Username:    sap.userID,
		// 		DeleteOp:    sap.opIsDelete,
		// 		OpName:      "custom",
		// 		Namespace:   "",
		// 		KubeConfigs: sap.kubeconfigs,
		// 	})

		// 	return resp.String(), err
		// }

		// Else it is an OAM adapter call
		//TODO: Accomodate gRPC calls to use context mapping with kubeconfig
		var kconfigs []string
		for _, v := range sap.ctxTokubeconfig {
			kconfigs = append(kconfigs, v)
		}
		resp, err := mClient.MClient.ProcessOAM(context.TODO(), &meshes.ProcessOAMRequest{
			Username:    sap.userID,
			DeleteOp:    sap.opIsDelete,
			OamComps:    []string{string(jsonComp)},
			OamConfig:   string(jsonConfig),
			KubeConfigs: kconfigs,
		})

		return resp.GetMessage(), err
	}

	return "", nil
}

func (sap *serviceActionProvider) Persist(name string, svc core.Service, isUpdate bool) error {
	if !sap.opIsDelete {
		if isUpdate {
			// Do nothing
			return nil
		}

		_, err := sap.provider.SaveMesheryPatternResource(
			sap.token,
			&models.PatternResource{
				ID:        svc.ID,
				Name:      name,
				Namespace: svc.Namespace,
				Type:      svc.Type,
				OAMType:   "workload",
			},
		)

		return err
	}

	return sap.provider.DeleteMesheryPatternResource(
		sap.token,
		svc.ID.String(),
	)
}
