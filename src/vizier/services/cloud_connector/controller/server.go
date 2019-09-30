package controllers

import (
	"context"
	"errors"
	"time"

	"github.com/gogo/protobuf/types"
	uuid "github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"

	"pixielabs.ai/pixielabs/src/cloud/cloudpb"
	"pixielabs.ai/pixielabs/src/cloud/vzconn/vzconnpb"
	"pixielabs.ai/pixielabs/src/utils"
	certmgrpb "pixielabs.ai/pixielabs/src/vizier/services/certmgr/certmgrpb"
)

const heartbeatIntervalS = 5 * time.Second
const heartbeatWaitS = 2 * time.Second

// VizierInfo fetches information about Vizier.
type VizierInfo interface {
	GetAddress() (string, error)
}

// Server defines an gRPC server type.
type Server struct {
	vzConnClient  vzconnpb.VZConnServiceClient
	certMgrClient certmgrpb.CertMgrServiceClient
	vizierID      uuid.UUID
	jwtSigningKey string
	hbSeqNum      int64
	clock         utils.Clock
	quitCh        chan bool
	vzInfo        VizierInfo
}

// NewServer creates GRPC handlers.
func NewServer(vizierID uuid.UUID, jwtSigningKey string, vzConnClient vzconnpb.VZConnServiceClient, certMgrClient certmgrpb.CertMgrServiceClient, vzInfo VizierInfo) *Server {
	clock := utils.SystemClock{}
	return NewServerWithClock(vizierID, jwtSigningKey, vzConnClient, certMgrClient, vzInfo, clock)
}

// NewServerWithClock creates a new server with the given clock.
func NewServerWithClock(vizierID uuid.UUID, jwtSigningKey string, vzConnClient vzconnpb.VZConnServiceClient, certMgrClient certmgrpb.CertMgrServiceClient, vzInfo VizierInfo, clock utils.Clock) *Server {
	return &Server{
		vizierID:      vizierID,
		jwtSigningKey: jwtSigningKey,
		vzConnClient:  vzConnClient,
		certMgrClient: certMgrClient,
		hbSeqNum:      0,
		clock:         clock,
		quitCh:        make(chan bool),
		vzInfo:        vzInfo,
	}
}

// DoWithTimeout runs f and returns its error.  If the deadline d elapses first,
// it returns a grpc DeadlineExceeded error instead.
func DoWithTimeout(f func() error, d time.Duration) error {
	errChan := make(chan error, 1)
	go func() {
		errChan <- f()
		close(errChan)
	}()
	t := time.NewTimer(d)
	select {
	case <-t.C:
		return errors.New("timeout")
	case err := <-errChan:
		if !t.Stop() {
			<-t.C
		}
		return err
	}
}

// StartStream starts the stream between the cloud connector and vizier connector.
func (s *Server) StartStream() {
	stream, err := s.vzConnClient.CloudConnect(context.Background())
	if err != nil {
		log.WithError(err).Fatal("Could not start stream")
	}

	err = s.RegisterVizier(stream)
	if err != nil {
		log.WithError(err).Fatal("Failed to register vizier")
	}

	// Request the SSL certs and then send them cert manager.
	// TODO(zasgar/michelle): In the future we should update this so that the
	// cert manager is the one who initiates cert requests.
	err = s.RequestAndHandleSSLCerts(stream)
	if err != nil {
		log.WithError(err).Fatal("Failed to fetch SSL certs")
	}

	// Send heartbeats.
	go s.doHeartbeats(stream)
}

func wrapRequest(p *types.Any, topic string) *vzconnpb.CloudConnectRequest {
	return &vzconnpb.CloudConnectRequest{
		Topic: topic,
		Msg:   p,
	}
}

// RegisterVizier registers the cluster with VZConn.
func (s *Server) RegisterVizier(stream vzconnpb.VZConnService_CloudConnectClient) error {
	addr, err := s.vzInfo.GetAddress()
	if err != nil {
		log.WithError(err).Info("Unable to get vizier proxy address")
	}

	// Send over a registration request and wait for ACK.
	regReq := &cloudpb.RegisterVizierRequest{
		VizierID: utils.ProtoFromUUID(&s.vizierID),
		JwtKey:   s.jwtSigningKey,
		Address:  addr,
	}

	anyMsg, err := types.MarshalAny(regReq)
	if err != nil {
		return err
	}
	wrappedReq := wrapRequest(anyMsg, "register")
	if err := stream.Send(wrappedReq); err != nil {
		return err
	}

	tries := 0
	for tries < 5 {
		err = DoWithTimeout(func() error {
			// Try to receive the registerAck.
			resp, err := stream.Recv()
			if err != nil {
				return err
			}
			registerAck := &cloudpb.RegisterVizierAck{}
			err = types.UnmarshalAny(resp.Msg, registerAck)
			if err != nil {
				return err
			}

			if registerAck.Status != cloudpb.ST_OK {
				return errors.New("vizier registration unsuccessful")
			}
			return nil
		}, heartbeatWaitS)

		if err == nil {
			return nil // Registered successfully.
		}
		tries++
	}

	return err
}

func (s *Server) doHeartbeats(stream vzconnpb.VZConnService_CloudConnectClient) {
	for {
		select {
		case <-s.quitCh:
			return
		default:
			s.HandleHeartbeat(stream)
			time.Sleep(heartbeatIntervalS)
		}
	}
}

// HandleHeartbeat sends a heartbeat to the VZConn and waits for a response.
func (s *Server) HandleHeartbeat(stream vzconnpb.VZConnService_CloudConnectClient) {
	addr, err := s.vzInfo.GetAddress()
	if err != nil {
		log.WithError(err).Info("Unable to get vizier proxy address")
	}

	hbMsg := cloudpb.VizierHeartbeat{
		VizierID:       utils.ProtoFromUUID(&s.vizierID),
		Time:           s.clock.Now().Unix(),
		SequenceNumber: s.hbSeqNum,
		Address:        addr,
	}

	hbMsgAny, err := types.MarshalAny(&hbMsg)
	if err != nil {
		log.WithError(err).Fatal("Could not marshal heartbeat message")
	}

	// TODO(zasgar/michelle): There should be a vizier specific topic.
	msg := wrapRequest(hbMsgAny, "heartbeat")
	if err := stream.Send(msg); err != nil {
		log.WithError(err).Fatal("Could not send heartbeat (will retry)")
		return
	}

	s.hbSeqNum++

	err = DoWithTimeout(func() error {
		resp, err := stream.Recv()
		if err != nil {
			return errors.New("Could not receive heartbeat ack")
		}
		hbAck := &cloudpb.VizierHeartbeatAck{}
		err = types.UnmarshalAny(resp.Msg, hbAck)
		if err != nil {
			return errors.New("Could not unmarshal heartbeat ack")
		}

		if hbAck.SequenceNumber != hbMsg.SequenceNumber {
			return errors.New("Received out of sequence heartbeat ack")
		}
		return nil
	}, heartbeatWaitS)

	if err != nil {
		log.WithError(err).Fatal("Did not receive heartbeat ack")
	}
}

// RequestAndHandleSSLCerts registers the cluster with VZConn.
func (s *Server) RequestAndHandleSSLCerts(stream vzconnpb.VZConnService_CloudConnectClient) error {
	// Send over a request for SSL certs.
	regReq := &cloudpb.VizierSSLCertRequest{
		VizierID: utils.ProtoFromUUID(&s.vizierID),
	}
	anyMsg, err := types.MarshalAny(regReq)
	if err != nil {
		return err
	}
	wrappedReq := wrapRequest(anyMsg, "ssl")
	if err := stream.Send(wrappedReq); err != nil {
		return err
	}

	resp, err := stream.Recv()
	if err != nil {
		return err
	}

	sslCertResp := &cloudpb.VizierSSLCertResponse{}
	err = types.UnmarshalAny(resp.Msg, sslCertResp)
	if err != nil {
		return err
	}

	certMgrReq := &certmgrpb.UpdateCertsRequest{
		Key:  sslCertResp.Key,
		Cert: sslCertResp.Cert,
	}
	crtMgrResp, err := s.certMgrClient.UpdateCerts(stream.Context(), certMgrReq)
	if err != nil {
		return err
	}

	if !crtMgrResp.OK {
		return errors.New("Failed to update certs")
	}
	return nil
}

// Stop stops the server and ends the heartbeats.
func (s *Server) Stop() {
	close(s.quitCh)
}
