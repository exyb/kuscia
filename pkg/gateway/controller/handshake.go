// Copyright 2023 Ant Group Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/cri-api/pkg/errors"

	"github.com/secretflow/kuscia/pkg/common"
	kusciaapisv1alpha1 "github.com/secretflow/kuscia/pkg/crd/apis/kuscia/v1alpha1"
	clientset "github.com/secretflow/kuscia/pkg/crd/clientset/versioned"
	"github.com/secretflow/kuscia/pkg/gateway/clusters"
	"github.com/secretflow/kuscia/pkg/gateway/utils"
	"github.com/secretflow/kuscia/pkg/gateway/xds"
	"github.com/secretflow/kuscia/pkg/utils/nlog"
	tlsutils "github.com/secretflow/kuscia/pkg/utils/tls"
	"github.com/secretflow/kuscia/proto/api/v1alpha1"
	"github.com/secretflow/kuscia/proto/api/v1alpha1/handshake"
)

const (
	tokenByteSize = 32
	NoopToken     = "noop"
)

const (
	// Deprecated
	handShakeTypeUID = "UID"
	handShakeTypeRSA = "RSA"
)

const (
	kusciaTokenRevision = "Kuscia-Token-Revision"
)

var (
	tokenPrefix = []byte("kuscia")
)

type Token struct {
	Token   string
	Version int64
}

type RevisionToken struct {
	RawToken       []byte
	PublicKey      *rsa.PublicKey
	ExpirationTime int64
	Revision       int32
}

type AfterRegisterDomainHook func(response *handshake.RegisterResponse)

func (c *DomainRouteController) startHandShakeServer(port uint32) {
	mux := http.NewServeMux()
	mux.HandleFunc(utils.GetHandshakePathSuffix(), c.handShakeHandle)
	if c.isMaser {
		mux.HandleFunc("/register", c.registerHandle)
	}

	c.handshakeServer = &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", port),
		Handler: mux,
	}

	nlog.Error(c.handshakeServer.ListenAndServe())
}

func doHTTPWithDefaultRetry(in interface{}, out interface{}, hp *utils.HTTPParam) error {
	return utils.DoHTTPWithRetry(in, out, hp, time.Second, 5)
}

func (c *DomainRouteController) waitTokenReady(drName string) error {
	maxRetryTimes := 30
	i := 0
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		i++
		drLatest, err := c.domainRouteLister.DomainRoutes(c.gateway.Namespace).Get(drName)
		if err != nil {
			return err
		}
		if drLatest.Status.TokenStatus.RevisionToken.IsReady {
			return nil
		}
		if drLatest.Status.TokenStatus.RevisionToken.Token == "" {
			return fmt.Errorf("token of dr %s was deleted ", drName)
		}
		if i == maxRetryTimes {
			break
		}
	}
	return fmt.Errorf("wait dr %s token ready failed at max retry times:%d", drName, maxRetryTimes)
}

func (c *DomainRouteController) checkConnectionStatus(dr *kusciaapisv1alpha1.DomainRoute, clusterName string) error {
	out := &getResponse{}
	headers := map[string]string{
		kusciaTokenRevision: fmt.Sprintf("%d", dr.Status.TokenStatus.RevisionToken.Revision),
	}

	handshakePath := utils.GetHandshakePathOfEndpoint(dr.Spec.Endpoint)
	if dr.Spec.Destination == c.getMasterNamespace() {
		handshakePath = utils.GetHandshakePathOfPrefix(c.getMasterProxyPath())
	}

	hp := &utils.HTTPParam{
		Method:       http.MethodGet,
		Path:         handshakePath,
		ClusterName:  clusterName,
		KusciaHost:   getHandshakeHost(dr),
		KusciaSource: dr.Spec.Source,
		Transit:      utils.IsTransit(dr.Spec.Transit),
		Headers:      headers}
	err := utils.DoHTTP(nil, out, hp)
	if err != nil {
		_ = c.markDestUnreachable(context.Background(), dr)
		return err
	}

	c.refreshHeartbeatTime(dr)
	_ = c.markDestReachable(context.Background(), dr)
	return c.handleGetResponse(out, dr)
}

func (c *DomainRouteController) handleGetResponse(out *getResponse, dr *kusciaapisv1alpha1.DomainRoute) error {
	switch out.State {
	case TokenReady:
		if !dr.Status.TokenStatus.RevisionToken.IsReady {
			revision := dr.Status.TokenStatus.RevisionToken.Revision
			dr = dr.DeepCopy()
			dr.Status.TokenStatus.RevisionToken.IsReady = true
			dr.Status.IsDestinationUnreachable = false
			_, err := c.kusciaClient.KusciaV1alpha1().DomainRoutes(dr.Namespace).UpdateStatus(context.Background(), dr, metav1.UpdateOptions{})
			if err == nil {
				nlog.Infof("Domainroute %s found destination token(revision %d) ready", dr.Name, revision)
			}
			return err
		}
		return nil
	case TokenNotFound:
		if dr.Status.TokenStatus.RevisionToken.Token != "" || dr.Status.TokenStatus.Tokens != nil || dr.Status.TokenStatus.RevisionToken.IsReady {
			revision := dr.Status.TokenStatus.RevisionToken.Revision
			dr = dr.DeepCopy()
			dr.Status.TokenStatus = kusciaapisv1alpha1.DomainRouteTokenStatus{}
			_, err := c.kusciaClient.KusciaV1alpha1().DomainRoutes(dr.Namespace).UpdateStatus(context.Background(), dr, metav1.UpdateOptions{})
			if err == nil {
				nlog.Infof("Domainroute %s found destination token(revision %d) not exist", dr.Name, revision)
			}
			return err
		}
		return nil
	case DomainIDInputInvalid:
		return fmt.Errorf("%s destinationreturn  DomainIDInputInvalid, check 'Kuscia-Source' in header", dr.Name)
	case TokenRevisionInputInvalid:
		return fmt.Errorf("%s destination return TokenRevisionInputInvalid, check 'Kuscia-Token-Revision' in header", dr.Name)
	case TokenNotReady:
		return fmt.Errorf("%s destination token is not ready", dr.Name)
	case NoAuthentication:
		if dr.Status.IsDestinationAuthorized {
			dr = dr.DeepCopy()
			dr.Status.IsDestinationAuthorized = false
			dr.Status.IsDestinationUnreachable = false
			dr.Status.TokenStatus = kusciaapisv1alpha1.DomainRouteTokenStatus{}
			_, _ = c.kusciaClient.KusciaV1alpha1().DomainRoutes(dr.Namespace).UpdateStatus(context.Background(), dr, metav1.UpdateOptions{})
			return fmt.Errorf("%s can't contact destination because destination authentication is false", dr.Name)
		}
		return nil
	case InternalError:
		return fmt.Errorf("%s destination return unknown error", dr.Name)
	default:
		return nil
	}
}

func calcPublicKeyHash(pubStr string) ([]byte, error) {
	srcPub, err := base64.StdEncoding.DecodeString(pubStr)
	if err != nil {
		return nil, err
	}
	msgHash := sha256.New()
	_, err = msgHash.Write(srcPub)
	if err != nil {
		return nil, err
	}
	return msgHash.Sum(nil), nil
}

func (c *DomainRouteController) sourceInitiateHandShake(dr *kusciaapisv1alpha1.DomainRoute, clusterName string) error {
	if dr.Spec.TokenConfig.SourcePublicKey != c.gateway.Status.PublicKey {
		nlog.Errorf("DomainRoute %s: mismatch source public key", dr.Name)
		return nil
	}

	handshakeReq := &handshake.HandShakeRequest{
		DomainId:    dr.Spec.Source,
		RequestTime: time.Now().UnixNano(),
	}

	//1. In UID mode, the token is directly generated by the peer end and encrypted by the local public key
	//2. In RSA mode, the local end and the peer end generate their own tokens and concatenate them.
	//   The local token is encrypted with the peer's public key and then sent.
	//   The peer token is encrypted with the local public key and returned.
	var token []byte
	resp := &handshake.HandShakeResponse{}
	if dr.Spec.TokenConfig.TokenGenMethod == kusciaapisv1alpha1.TokenGenUIDRSA {
		handshakeReq.Type = handShakeTypeUID
		handshakePath := utils.GetHandshakePathOfEndpoint(dr.Spec.Endpoint)
		if dr.Spec.Destination == c.getMasterNamespace() {
			handshakePath = utils.GetHandshakePathOfPrefix(c.getMasterProxyPath())
		}
		err := doHTTPWithDefaultRetry(handshakeReq, resp, &utils.HTTPParam{
			Method:       http.MethodPost,
			Path:         handshakePath,
			KusciaSource: dr.Spec.Source,
			ClusterName:  clusterName,
			KusciaHost:   getHandshakeHost(dr),
			Transit:      utils.IsTransit(dr.Spec.Transit),
		})
		if err != nil {
			nlog.Errorf("DomainRoute %s: handshake fail:%v", dr.Name, err)
			return err
		}
		if resp.Status.Code != 0 {
			err = fmt.Errorf("DomainRoute %s: handshake fail, return error:%v", dr.Name, resp.Status.Message)
			nlog.Error(err)
			return err
		}
		token, err = decryptToken(c.prikey, resp.Token.Token, tokenByteSize)
		if err != nil {
			err = fmt.Errorf("DomainRoute %s: handshake fail, return error:%v", dr.Name, resp.Status.Message)
			nlog.Error(err)
			return err
		}
	} else if dr.Spec.TokenConfig.TokenGenMethod == kusciaapisv1alpha1.TokenGenMethodRSA {
		handshakeReq.Type = handShakeTypeRSA

		msgHashSum, err := calcPublicKeyHash(c.gateway.Status.PublicKey)
		if err != nil {
			return err
		}

		sourceToken := make([]byte, tokenByteSize/2)
		_, err = rand.Read(sourceToken)
		if err != nil {
			return err
		}

		//Resolve the public key of the peer end from domainroute crd
		destPub, err := base64.StdEncoding.DecodeString(dr.Spec.TokenConfig.DestinationPublicKey)
		if err != nil {
			nlog.Errorf("DomainRoute %s: destination public key format error, must be base64 encoded", dr.Name)
			return err
		}
		destPubKey, err := tlsutils.ParseRSAPublicKey(destPub)
		if err != nil {
			return err
		}

		sourceTokenEnc, err := encryptToken(destPubKey, sourceToken)
		if err != nil {
			return err
		}

		handshakeReq.TokenConfig = &handshake.TokenConfig{
			Token:    sourceTokenEnc,
			Revision: dr.Status.TokenStatus.RevisionToken.Revision,
			Pubhash:  base64.StdEncoding.EncodeToString(msgHashSum),
		}

		handshakePath := utils.GetHandshakePathOfEndpoint(dr.Spec.Endpoint)
		if dr.Spec.Destination == c.getMasterNamespace() {
			handshakePath = utils.GetHandshakePathOfPrefix(c.getMasterProxyPath())
		}
		err = doHTTPWithDefaultRetry(handshakeReq, resp, &utils.HTTPParam{
			Method:       http.MethodPost,
			Path:         handshakePath,
			KusciaSource: dr.Spec.Source,
			ClusterName:  clusterName,
			KusciaHost:   getHandshakeHost(dr),
			Transit:      utils.IsTransit(dr.Spec.Transit),
		})
		if err != nil {
			nlog.Warnf("DomainRoute %s: handshake fail:%v", dr.Name, err)
			return err
		}
		if resp.Status == nil {
			err = fmt.Errorf("DomainRoute %s: handshake fail, status is null, some unexcepted cases occurs", dr.Name)
			nlog.Warn(err)
			return err
		}
		if resp.Status.Code != 0 {
			err = fmt.Errorf("DomainRoute %s: handshake fail, return error:%v", dr.Name, resp.Status.Message)
			nlog.Warn(err)
			return err
		}
		destToken, err := decryptToken(c.prikey, resp.Token.Token, tokenByteSize/2)
		if err != nil {
			err = fmt.Errorf("DomainRoute %s: handshake fail, decryptToken  error:%v", dr.Name, resp.Status.Message)
			nlog.Warn(err)
			return err
		}
		token = append(sourceToken, destToken...)
	} else {
		return fmt.Errorf("TokenGenMethod must be %s or %s", kusciaapisv1alpha1.TokenGenUIDRSA, kusciaapisv1alpha1.TokenGenMethodRSA)
	}

	// The final token is encrypted with the local private key and stored in the status of domainroute
	revisionToken := &RevisionToken{
		RawToken:       token,
		PublicKey:      &c.prikey.PublicKey,
		Revision:       resp.Token.Revision,
		ExpirationTime: resp.Token.ExpirationTime,
	}

	return UpdateDomainRouteRevisionToken(c.kusciaClient, dr.Namespace, dr.Name, revisionToken)
}

func UpdateDomainRouteRevisionToken(kusciaClient clientset.Interface, namespace, drName string, revisionToken *RevisionToken) error {
	tokenEncrypted, err := encryptToken(revisionToken.PublicKey, revisionToken.RawToken)
	if err != nil {
		return err
	}
	drLatest, err := kusciaClient.KusciaV1alpha1().DomainRoutes(namespace).Get(context.Background(), drName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	tn := metav1.Now()
	drUpdate := drLatest.DeepCopy()
	drUpdateStatus := &drUpdate.Status
	drUpdateRevisionToken := &drUpdateStatus.TokenStatus.RevisionToken
	drUpdateStatus.IsDestinationAuthorized = true
	drUpdateRevisionToken.Token = tokenEncrypted
	drUpdateRevisionToken.Revision = int64(revisionToken.Revision)
	drUpdateRevisionToken.IsReady = true
	drUpdateRevisionToken.RevisionTime = tn
	if drUpdate.Spec.TokenConfig.RollingUpdatePeriod == 0 {
		drUpdateRevisionToken.ExpirationTime = metav1.NewTime(tn.AddDate(100, 0, 0))
	} else {
		expirationTime := time.Unix(revisionToken.ExpirationTime/int64(time.Second), revisionToken.ExpirationTime%int64(time.Second))
		drUpdateRevisionToken.ExpirationTime = metav1.NewTime(expirationTime)
	}

	_, err = kusciaClient.KusciaV1alpha1().DomainRoutes(drUpdate.Namespace).UpdateStatus(context.Background(), drUpdate, metav1.UpdateOptions{})
	return err
}

type DestinationStatus int

const (
	TokenReady DestinationStatus = iota
	DomainIDInputInvalid
	TokenRevisionInputInvalid
	TokenNotReady
	TokenNotFound
	NoAuthentication
	InternalError
	NetworkUnreachable
)

type getResponse struct {
	Namespace string            `json:"namespace"`
	State     DestinationStatus `json:"state"`
}

func (c *DomainRouteController) handShakeHandle(w http.ResponseWriter, r *http.Request) {
	nlog.Debugf("Receive handshake request, method [%s], host[%s], headers[%s]", r.Method, r.Host, r.Header)
	if r.Method == http.MethodGet {
		resp := &getResponse{
			Namespace: c.gateway.Namespace,
			State:     TokenNotReady,
		}

		domainID := r.Header.Get("Kuscia-Origin-Source")
		if domainID == "" {
			domainID = r.Header.Get("Kuscia-Source")
		}
		tokenRevision := r.Header.Get(kusciaTokenRevision)
		resp.State = c.checkTokenStatus(domainID, tokenRevision)

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(resp)
		if err != nil {
			nlog.Errorf("write handshake response fail, detail-> %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	req := handshake.HandShakeRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		nlog.Errorf("Invalid request: %v", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	drName := common.GenDomainRouteName(req.DomainId, c.gateway.Namespace)
	resp := c.DestReplyHandshake(&req, drName)
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		nlog.Errorf("encode handshake response for(%s) fail, detail-> %v", drName, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} else {
		if resp.Status.Code != 0 {
			nlog.Errorf("DestReplyHandshake for [%s] failed, detail-> %v", drName, resp.Status.Message)
		} else {
			nlog.Infof("DomainRoute %s handle successfully", drName)
		}
	}
}

func (c *DomainRouteController) checkTokenStatus(domainID, tokenRevision string) DestinationStatus {
	if domainID == "" {
		return DomainIDInputInvalid
	}
	drName := common.GenDomainRouteName(domainID, c.gateway.Namespace)
	if dr, err := c.domainRouteLister.DomainRoutes(c.gateway.Namespace).Get(drName); err == nil {
		destStatus := checkTokenRevision(tokenRevision, dr)
		if destStatus == TokenReady {
			index, token := IndexToken(tokenRevision, dr)
			token.HeartBeatTime = metav1.Now()
			dr.Status.TokenStatus.Tokens[index] = token
			if _, err = c.kusciaClient.KusciaV1alpha1().DomainRoutes(dr.Namespace).UpdateStatus(context.Background(), dr, metav1.UpdateOptions{}); err != nil {
				nlog.Errorf("update domain route fail, detail -> %v", err)
			}
		}
		return destStatus
	} else if k8serrors.IsNotFound(err) {
		return NoAuthentication
	}
	return InternalError
}

func checkTokenRevision(tokenRevision string, dr *kusciaapisv1alpha1.DomainRoute) DestinationStatus {
	if tokenRevision == "" {
		return TokenRevisionInputInvalid
	}

	index, token := IndexToken(tokenRevision, dr)
	if index == -1 {
		return TokenNotFound
	}
	if token.IsReady {
		return TokenReady
	}
	return TokenNotReady
}

func IndexToken(revision string, dr *kusciaapisv1alpha1.DomainRoute) (int, kusciaapisv1alpha1.DomainRouteToken) {
	for index, t := range dr.Status.TokenStatus.Tokens {
		if strconv.FormatInt(t.Revision, 10) == revision {
			return index, t
		}
	}
	return -1, kusciaapisv1alpha1.DomainRouteToken{}
}

func buildFailedHandshakeReply(code int32, err error) *handshake.HandShakeResponse {
	resp := &handshake.HandShakeResponse{
		Status: &v1alpha1.Status{
			Code: code,
		},
	}
	if err != nil {
		resp.Status.Message = err.Error()
	}
	return resp
}

func (c *DomainRouteController) DestReplyHandshake(req *handshake.HandShakeRequest, drName string) *handshake.HandShakeResponse {
	srcDomain := req.DomainId
	destDomain := c.gateway.Namespace
	dr, err := c.domainRouteLister.DomainRoutes(destDomain).Get(drName)
	if err != nil {
		if errors.IsNotFound(err) {
			return buildFailedHandshakeReply(500, fmt.Errorf("domainRoute [%s] not found in dest domain [%s]", drName, destDomain))
		}
		return buildFailedHandshakeReply(500, fmt.Errorf("domainRoute [%s] get error in dest domain [%s]: %s", drName, destDomain, err.Error()))
	}
	if req.Type != handShakeTypeRSA && req.Type != handShakeTypeUID {
		return buildFailedHandshakeReply(500, fmt.Errorf("invalid handshake type [%s]", req.Type))
	}
	if !(req.Type == handShakeTypeUID && dr.Spec.TokenConfig.TokenGenMethod == kusciaapisv1alpha1.TokenGenUIDRSA) &&
		!(req.Type == handShakeTypeRSA && dr.Spec.TokenConfig.TokenGenMethod == kusciaapisv1alpha1.TokenGenMethodRSA) {
		// From Kuscia 0.14, for Lite we support both UID and RSA. And default is RSA.
		// This should be only for compatibility when a master (>=0.14) connects with a Lite (<0.14) by using default add_domain_lite script.
		// In other cases, the handshake type should still be the same as the domainroute.
		nlog.Warnf("handshake type [%s] mismatch in domainroute [%s], ignored and use req.Type[%s].",
			req.Type, dr.Spec.TokenConfig.TokenGenMethod, req.Type)
		nlog.Warnf("check domainroute setting or use same version kuscia(>=0.14) for both master and lite.")
	}
	srcPub, err := base64.StdEncoding.DecodeString(dr.Spec.TokenConfig.SourcePublicKey)
	if err != nil {
		return buildFailedHandshakeReply(500, fmt.Errorf("invalid source domain [%s] publickey in domainroute [%s], must be based64 encoded string", srcPub, drName))
	}
	sourcePubKey, err := tlsutils.ParseRSAPublicKey(srcPub)
	if err != nil {
		return buildFailedHandshakeReply(500, fmt.Errorf("invalid source domain [%s] publickey in domainroute [%s], error: %s", srcDomain, drName, err.Error()))
	}
	dstRevisionToken := dr.Status.TokenStatus.RevisionToken

	var token []byte
	var respToken []byte
	if req.Type == handShakeTypeUID {
		// If the token in domainroute is empty or has expired, the token is regenerated.
		// Otherwise, the token is returned
		needGenerateToken := func() bool {
			if dstRevisionToken.Token == "" {
				return true
			}
			if dr.Spec.TokenConfig != nil && dr.Spec.TokenConfig.RollingUpdatePeriod > 0 && time.Since(dstRevisionToken.RevisionTime.Time) > time.Duration(dr.Spec.TokenConfig.RollingUpdatePeriod)*time.Second {
				return true
			}
			return false
		}
		if needGenerateToken() {
			respToken, err = generateRandomToken(tokenByteSize)
			if err != nil {
				return buildFailedHandshakeReply(500, fmt.Errorf("generate auth token in dest domain [%s] error: %s", destDomain, err.Error()))
			}
		} else {
			respToken, err = decryptToken(c.prikey, dstRevisionToken.Token, tokenByteSize)
			if err != nil {
				nlog.Warnf("decrypt token with revision [%d] in dest domain [%s] error: %s", dstRevisionToken.Revision, destDomain, err.Error())
				respToken, err = generateRandomToken(tokenByteSize)
				if err != nil {
					return buildFailedHandshakeReply(500, fmt.Errorf("generate auth token in dest domain [%s] error: %s", destDomain, err.Error()))
				}
			}
		}

		token = respToken
	} else if req.Type == handShakeTypeRSA {
		msgHashSum, calErr := calcPublicKeyHash(dr.Spec.TokenConfig.SourcePublicKey)
		if calErr != nil {
			return buildFailedHandshakeReply(500, fmt.Errorf("calculate source domain [%s] publickey hash error: %s", srcDomain, calErr.Error()))
		}
		if req.TokenConfig.Pubhash != base64.StdEncoding.EncodeToString(msgHashSum) {
			return buildFailedHandshakeReply(500, fmt.Errorf("source domain [%s] publickey hash mismatch in domainroute [%s]", srcDomain, dr.Name))
		}
		sourceToken, decryptErr := decryptToken(c.prikey, req.TokenConfig.Token, tokenByteSize/2)
		if decryptErr != nil {
			nlog.Errorf("dest domain [%s] publickey in source domain [%s] may be not correct, error: %s", destDomain, srcDomain, decryptErr.Error())
			return buildFailedHandshakeReply(500, fmt.Errorf("dest domain [%s] publickey in source domain [%s] may be not correct", destDomain, srcDomain))
		}

		respToken = make([]byte, tokenByteSize/2)
		if _, err = rand.Read(respToken); err != nil {
			return buildFailedHandshakeReply(500, fmt.Errorf("generate response auth token in dest domain [%s] error: %s", destDomain, err.Error()))
		}

		token = append(sourceToken, respToken...)
	}

	tokenEncrypted, err := encryptToken(&c.prikey.PublicKey, token)
	if err != nil {
		return buildFailedHandshakeReply(500, fmt.Errorf("encrypt auth token with dest domain [%s] publickey error: %s", destDomain, err.Error()))
	}

	respTokenEncrypted, err := encryptToken(sourcePubKey, respToken)
	if err != nil {
		return buildFailedHandshakeReply(500, fmt.Errorf("encrypt auth token with source domain [%s] publickey error: %s", srcDomain, err.Error()))
	}

	drLatest, err := c.domainRouteLister.DomainRoutes(dr.Namespace).Get(dr.Name)
	if err != nil {
		return buildFailedHandshakeReply(500, fmt.Errorf("get latest domainRoute [%s] in dest domain [%s] error: %s", drName, destDomain, err.Error()))
	}
	var revision int64
	var expirationTime metav1.Time
	if drLatest.Status.TokenStatus.RevisionToken.Token != tokenEncrypted {
		drCopy := drLatest.DeepCopy()
		revisionTime := metav1.Now()
		drCopy.Status.TokenStatus.RevisionToken.Token = tokenEncrypted
		drCopy.Status.TokenStatus.RevisionToken.Revision++
		revision = drCopy.Status.TokenStatus.RevisionToken.Revision
		drCopy.Status.TokenStatus.RevisionToken.RevisionTime = revisionTime
		drCopy.Status.TokenStatus.RevisionToken.IsReady = false
		if drCopy.Spec.TokenConfig.RollingUpdatePeriod == 0 {
			expirationTime = metav1.NewTime(revisionTime.AddDate(100, 0, 0))
		} else {
			expirationTime = metav1.NewTime(revisionTime.Add(2 * time.Duration(drCopy.Spec.TokenConfig.RollingUpdatePeriod) * time.Second))
		}
		drCopy.Status.TokenStatus.RevisionToken.ExpirationTime = expirationTime
		_, err = c.kusciaClient.KusciaV1alpha1().DomainRoutes(drCopy.Namespace).UpdateStatus(context.Background(), drCopy, metav1.UpdateOptions{})
		if err != nil {
			return buildFailedHandshakeReply(500, fmt.Errorf("update domainRoute [%s] in dest domain [%s] error: %s", drName, destDomain, err.Error()))
		}
		nlog.Infof("Update domainRoute [%s] status successfully", drName)
	} else {
		revision = drLatest.Status.TokenStatus.RevisionToken.Revision
		expirationTime = drLatest.Status.TokenStatus.RevisionToken.ExpirationTime
	}

	err = c.waitTokenReady(drLatest.Name)
	if err != nil {
		return buildFailedHandshakeReply(500, fmt.Errorf("wait token ready in dest domain [%s] error: %s", destDomain, err.Error()))
	}

	return &handshake.HandShakeResponse{
		Status: &v1alpha1.Status{
			Code: 0,
		},
		Token: &handshake.Token{
			Token:          respTokenEncrypted,
			ExpirationTime: expirationTime.UnixNano(),
			Revision:       int32(revision),
		},
	}
}

func (c *DomainRouteController) parseToken(dr *kusciaapisv1alpha1.DomainRoute, routeKey string) ([]*Token, error) {
	var tokens []*Token
	var err error

	is3rdParty := utils.IsThirdPartyTransit(dr.Spec.Transit)
	if (is3rdParty && dr.Spec.BodyEncryption == nil) ||
		(!is3rdParty && dr.Spec.AuthenticationType == kusciaapisv1alpha1.DomainAuthenticationMTLS) ||
		(!is3rdParty && dr.Spec.AuthenticationType == kusciaapisv1alpha1.DomainAuthenticationNone) {
		tokens = append(tokens, &Token{Token: NoopToken})
		return tokens, err
	}

	if (!is3rdParty && dr.Spec.AuthenticationType != kusciaapisv1alpha1.DomainAuthenticationToken) ||
		dr.Spec.TokenConfig == nil {
		return tokens, fmt.Errorf("invalid DomainRoute: %v", dr.Spec)
	}

	switch dr.Spec.TokenConfig.TokenGenMethod {
	case kusciaapisv1alpha1.TokenGenMethodRSA:
		tokens, err = c.parseTokenRSA(dr, false)
	case kusciaapisv1alpha1.TokenGenUIDRSA:
		tokens, err = c.parseTokenRSA(dr, true)
	default:
		err = fmt.Errorf("DomainRoute %s unsupported token method: %s", routeKey,
			dr.Spec.TokenConfig.TokenGenMethod)
	}
	return tokens, err
}

func (c *DomainRouteController) parseTokenRSA(dr *kusciaapisv1alpha1.DomainRoute, drop bool) ([]*Token, error) {
	key, _ := cache.MetaNamespaceKeyFunc(dr)

	var tokens []*Token
	if len(dr.Status.TokenStatus.Tokens) == 0 {
		return tokens, nil
	}

	if (c.gateway.Namespace == dr.Spec.Source && dr.Spec.TokenConfig.SourcePublicKey != c.gateway.Status.PublicKey) ||
		(c.gateway.Namespace == dr.Spec.Destination && dr.Spec.TokenConfig.DestinationPublicKey != c.gateway.Status.PublicKey) {
		err := fmt.Errorf("DomainRoute %s mismatch public key", key)
		return tokens, err
	}

	for _, token := range dr.Status.TokenStatus.Tokens {
		b, err := decryptToken(c.prikey, token.Token, tokenByteSize)
		if err != nil {
			if !drop {
				return []*Token{}, fmt.Errorf("DomainRoute %s decrypt token error: %v", key, err)
			}
			nlog.Warnf("DomainRoute %s decrypt token [revision -> %d] error: %v", key, token.Revision, err)
			continue
		}
		tokens = append(tokens, &Token{Token: base64.StdEncoding.EncodeToString(b), Version: token.Revision})
	}

	return tokens, nil
}

func (c *DomainRouteController) checkAndUpdateTokenInstances(dr *kusciaapisv1alpha1.DomainRoute) error {
	if len(dr.Status.TokenStatus.Tokens) == 0 {
		return nil
	}
	updated := false
	drCopy := dr.DeepCopy()
	for i := range drCopy.Status.TokenStatus.Tokens {
		if !exists(drCopy.Status.TokenStatus.Tokens[i].EffectiveInstances, c.gateway.Name) {
			updated = true
			drCopy.Status.TokenStatus.Tokens[i].EffectiveInstances = append(drCopy.Status.TokenStatus.Tokens[i].EffectiveInstances, c.gateway.Name)
		}
	}
	if updated {
		_, err := c.kusciaClient.KusciaV1alpha1().DomainRoutes(drCopy.Namespace).UpdateStatus(context.Background(), drCopy, metav1.UpdateOptions{})
		return err
	}
	return nil
}

func generateRandomToken(size int) ([]byte, error) {
	respToken := make([]byte, size)
	if _, err := rand.Read(respToken); err != nil {
		return nil, err
	}
	return respToken, nil
}

func encryptToken(pub *rsa.PublicKey, key []byte) (string, error) {
	return tlsutils.EncryptPKCS1v15(pub, key, tokenPrefix)
}

func decryptToken(priv *rsa.PrivateKey, ciphertext string, keysize int) ([]byte, error) {
	return tlsutils.DecryptPKCS1v15(priv, ciphertext, keysize, tokenPrefix)
}

func exists(slice []string, val string) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

func HandshakeToMaster(domainID string, pathPrefix string, prikey *rsa.PrivateKey, masterPubKey *rsa.PublicKey) (*RevisionToken, error) {
	handshakeReq, sourceToken, err := prepareHandshakeReq(domainID, pathPrefix, prikey, masterPubKey)

	resp := &handshake.HandShakeResponse{}

	handshakePath := utils.GetHandshakePathOfPrefix(pathPrefix)
	maxRetryTimes := 50
	for i := 0; i < maxRetryTimes; i++ {
		resp = &handshake.HandShakeResponse{}
		httpErr := utils.DoHTTP(handshakeReq, resp, &utils.HTTPParam{
			Method:       http.MethodPost,
			Path:         handshakePath,
			KusciaSource: domainID,
			ClusterName:  clusters.GetMasterClusterName(),
			KusciaHost:   fmt.Sprintf("%s.master.svc", utils.ServiceHandshake),
		})
		if httpErr != nil {
			nlog.Warn(httpErr)
		} else {
			if resp.Status.Code == 0 {
				break
			} else {
				nlog.Warn(resp.Status.Message)
			}
		}
		time.Sleep(time.Second)
	}

	if resp.Status.Code != 0 {
		respErr := fmt.Errorf("handshake to master fail, return error:%v", resp.Status.Message)
		nlog.Error(respErr)
		return nil, respErr
	}
	var token []byte
	var decryptErr error
	if handshakeReq.Type == handShakeTypeRSA {
		token, decryptErr = decryptToken(prikey, resp.Token.Token, tokenByteSize/2)
	} else {
		token, decryptErr = decryptToken(prikey, resp.Token.Token, tokenByteSize)
	}
	if decryptErr != nil {
		nlog.Errorf("decrypt auth token from master error: %s", decryptErr.Error())
		return nil, decryptErr
	}
	token = append(sourceToken, token...)

	err = addMasterClusters(domainID, pathPrefix, token)
	if err != nil {
		return nil, err
	}

	return &RevisionToken{
		RawToken:       token,
		PublicKey:      &prikey.PublicKey,
		ExpirationTime: resp.Token.ExpirationTime,
		Revision:       resp.Token.Revision,
	}, nil
}

func prepareHandshakeReq(domainID string, pathPrefix string, prikey *rsa.PrivateKey, masterPubKey *rsa.PublicKey) (*handshake.HandShakeRequest, []byte, error) {
	handshakeReq := &handshake.HandShakeRequest{
		DomainId:    domainID,
		RequestTime: time.Now().UnixNano(),
	}

	//1. In UID mode[deprecated], the token is directly generated by the peer end and encrypted by the local public key
	//2. In RSA mode, the local end and the peer end generate their own tokens and concatenate them.
	//   The local token is encrypted with the peer's public key and then sent.
	//   The peer token is encrypted with the local public key and returned.
	var sourceToken []byte
	nlog.Debugf("prepare to handshake to master, domainID: %s, pathPrefix: %s, masterPubKey: %v", domainID, pathPrefix, masterPubKey)
	if masterPubKey != nil {
		pubKey := base64.StdEncoding.EncodeToString(tlsutils.EncodePKCS1PublicKey(prikey))
		msgHashSum, err := calcPublicKeyHash(pubKey)
		if err != nil {
			return nil, nil, err
		}

		sourceToken = make([]byte, tokenByteSize/2)
		_, err = rand.Read(sourceToken)
		if err != nil {
			return nil, nil, err
		}

		sourceTokenEnc, err := encryptToken(masterPubKey, sourceToken)
		if err != nil {
			return nil, nil, err
		}

		handshakeReq.TokenConfig = &handshake.TokenConfig{
			Token:    sourceTokenEnc,
			Revision: 0,
			Pubhash:  base64.StdEncoding.EncodeToString(msgHashSum),
		}

		handshakeReq.Type = handShakeTypeRSA
	} else {
		// for compatibility
		handshakeReq.Type = handShakeTypeUID
	}
	return handshakeReq, sourceToken, nil
}

func addMasterClusters(domainID string, pathPrefix string, token []byte) error {
	c, err := xds.QueryCluster(clusters.GetMasterClusterName())
	if err != nil {
		nlog.Error(err)
		return err
	}
	if err = clusters.AddMasterProxyVirtualHost(c.Name, pathPrefix, utils.ServiceMasterProxy, domainID, base64.StdEncoding.EncodeToString(token), []string{"*.master.svc"}); err != nil {
		nlog.Error(err)
		return err
	}
	if err = xds.SetKeepAliveForDstCluster(c, true); err != nil {
		nlog.Error(err)
		return err
	}
	if err = xds.AddOrUpdateCluster(c); err != nil {
		nlog.Error(err)
		return err
	}
	return nil
}
