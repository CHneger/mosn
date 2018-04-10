package sofarpc

import (
	"gitlab.alipay-inc.com/afe/mosn/pkg/log"
	"gitlab.alipay-inc.com/afe/mosn/pkg/protocol"
	"gitlab.alipay-inc.com/afe/mosn/pkg/protocol/sofarpc"
	str "gitlab.alipay-inc.com/afe/mosn/pkg/stream"
	"gitlab.alipay-inc.com/afe/mosn/pkg/types"
	"strconv"
	"sync"
)

func init() {
	str.Register(protocol.SofaRpc, &streamConnFactory{})
}

type streamConnFactory struct{}

func (f *streamConnFactory) CreateClientStream(connection types.ClientConnection,
	clientCallbacks types.StreamConnectionCallbacks, connCallbacks types.ConnectionCallbacks) types.ClientStreamConnection {
	return newStreamConnection(connection, clientCallbacks, nil)
}

func (f *streamConnFactory) CreateServerStream(connection types.Connection,
	serverCallbacks types.ServerStreamConnectionCallbacks) types.ServerStreamConnection {
	return newStreamConnection(connection, nil, serverCallbacks)
}

func (f *streamConnFactory) CreateBiDirectStream(connection types.ClientConnection, clientCallbacks types.StreamConnectionCallbacks,
	serverCallbacks types.ServerStreamConnectionCallbacks) types.ClientStreamConnection {
	return newStreamConnection(connection, clientCallbacks, serverCallbacks)
}

// types.DecodeFilter
// types.StreamConnection
// types.ClientStreamConnection
// types.ServerStreamConnection
type streamConnection struct {
	protocol        types.Protocol
	connection      types.Connection
	activeStreams   map[uint32]*stream
	asMutex         sync.Mutex
	protocols       types.Protocols
	clientCallbacks types.StreamConnectionCallbacks
	serverCallbacks types.ServerStreamConnectionCallbacks
}

func newStreamConnection(connection types.Connection, clientCallbacks types.StreamConnectionCallbacks,
	serverCallbacks types.ServerStreamConnectionCallbacks) types.ClientStreamConnection {

	return &streamConnection{
		connection:      connection,
		protocols:       sofarpc.DefaultProtocols(),
		activeStreams:   make(map[uint32]*stream),
		clientCallbacks: clientCallbacks,
		serverCallbacks: serverCallbacks,
	}
}

// types.StreamConnection
func (conn *streamConnection) Dispatch(buffer types.IoBuffer) {
	conn.protocols.Decode(buffer, conn)
}

func (conn *streamConnection) Protocol() types.Protocol {
	return conn.protocol
}

func (conn *streamConnection) OnUnderlyingConnectionAboveWriteBufferHighWatermark() {
	// todo
}

func (conn *streamConnection) OnUnderlyingConnectionBelowWriteBufferLowWatermark() {
	// todo
}

func (conn *streamConnection) NewStream(streamId uint32, responseDecoder types.StreamDecoder) types.StreamEncoder {
	stream := &stream{
		streamId:   streamId,
		direction:  0, //out
		connection: conn,
		decoder:    responseDecoder,
	}

	conn.activeStreams[streamId] = stream

	return stream
}

// types.DecodeFilter Called by serverStreamConnection
func (conn *streamConnection) OnDecodeHeader(streamId uint32, headers map[string]string) types.FilterStatus {
	if sofarpc.IsSofaRequest(headers) {
		conn.onNewStreamDetected(streamId)
	}

	if v, ok := headers[sofarpc.SofaPropertyHeader("requestid")]; ok {
		headers[types.MosnStreamID] = v
	}

	if v, ok := headers[sofarpc.SofaPropertyHeader("timeout")]; ok {
		headers[types.MosnTryTimeout] = v
	}

	if v, ok := headers[sofarpc.SofaPropertyHeader("globaltimeout")]; ok {
		headers[types.MosnGlobalTimeout] = v
	}

	if stream, ok := conn.activeStreams[streamId]; ok {
		stream.decoder.OnDecodeHeaders(headers, false) //Call Back Proxy-Level's OnDecodeHeaders
	}

	return types.Continue
}

func (conn *streamConnection) OnDecodeData(streamId uint32, data types.IoBuffer) types.FilterStatus {
	if stream, ok := conn.activeStreams[streamId]; ok {
		stream.decoder.OnDecodeData(data, true)

		if stream.direction == 0 {
			delete(stream.connection.activeStreams, stream.streamId)
		}
	}

	return types.StopIteration
}

func (conn *streamConnection) OnDecodeTrailer(streamId uint32, trailers map[string]string) types.FilterStatus {
	if stream, ok := conn.activeStreams[streamId]; ok {
		stream.decoder.OnDecodeTrailers(trailers)
	}

	return types.StopIteration
}

func (conn *streamConnection) onNewStreamDetected(streamId uint32) {
	if _, ok := conn.activeStreams[streamId]; ok {
		return
	}

	stream := &stream{
		streamId:   streamId,
		direction:  1, //in
		connection: conn,
	}

	stream.decoder = conn.serverCallbacks.NewStream(streamId, stream)
	conn.activeStreams[streamId] = stream
}

// types.Stream
// types.StreamEncoder
type stream struct {
	streamId         uint32
	direction        int // 0: out, 1: in
	readDisableCount int
	connection       *streamConnection
	decoder          types.StreamDecoder
	streamCbs        []types.StreamCallbacks
	encodedHeaders   types.IoBuffer
	encodedData      types.IoBuffer
}

// ~~ types.Stream
func (s *stream) AddCallbacks(cb types.StreamCallbacks) {
	s.streamCbs = append(s.streamCbs, cb)
}

func (s *stream) RemoveCallbacks(cb types.StreamCallbacks) {
	cbIdx := -1

	for i, streamCb := range s.streamCbs {
		if streamCb == cb {
			cbIdx = i
			break
		}
	}

	if cbIdx > -1 {
		s.streamCbs = append(s.streamCbs[:cbIdx], s.streamCbs[cbIdx+1:]...)
	}
}

func (s *stream) ResetStream(reason types.StreamResetReason) {
	for _, cb := range s.streamCbs {
		cb.OnResetStream(reason)
	}
}

func (s *stream) ReadDisable(disable bool) {
	s.connection.connection.SetReadDisable(disable)
}

func (s *stream) BufferLimit() uint32 {
	return s.connection.connection.BufferLimit()
}

// types.StreamEncoder
func (s *stream) EncodeHeaders(headers interface{}, endStream bool) {
	if headerMaps, ok := headers.(map[string]string); ok {

		// remove proxy header before codec encode
		if _, ok := headerMaps[types.MosnStreamID]; ok {
			delete(headerMaps, types.MosnStreamID)
		}

		if _, ok := headerMaps[types.MosnGlobalTimeout]; ok {
			delete(headerMaps, types.MosnGlobalTimeout)
		}

		if _, ok := headerMaps[types.MosnTryTimeout]; ok {
			delete(headerMaps, types.MosnTryTimeout)
		}

		if status, ok := headerMaps[types.HeaderStatus]; ok {

			delete(headerMaps, types.HeaderStatus)
			statusCode, _ := strconv.Atoi(status)

			if statusCode != 200 {
				// todo: handle proxy hijack reply on exception @boqin

				//Build Router Unavailable Response Msg
				if statusCode == 404 {
					respHeaders, err := sofarpc.BuildSofaRespMsg(headerMaps, sofarpc.RESPONSE_STATUS_UNKNOWN)
					if err == nil {
						switch respHeaders.(type) {
						case *sofarpc.BoltResponseCommand:
							headers = respHeaders.(*sofarpc.BoltResponseCommand)
						case *sofarpc.BoltV2ResponseCommand:
							headers = respHeaders.(*sofarpc.BoltV2ResponseCommand)
						default:
							headers = headerMaps
						}
					} else {
						log.DefaultLogger.Println(err.Error())
						headers = headerMaps
					}
				} //Other exception code
			} else {

				headers = headerMaps
			}

		} else {
			headers = headerMaps
		}
	}

	// Call Protocol-Level's EncodeHeaders Func
	s.streamId, s.encodedHeaders = s.connection.protocols.EncodeHeaders(headers)
	s.connection.activeStreams[s.streamId] = s

	if endStream {
		s.endStream()
	}
}

func (s *stream) EncodeData(data types.IoBuffer, endStream bool) {
	s.encodedData = data

	if endStream {
		s.endStream()
	}
}

func (s *stream) EncodeTrailers(trailers map[string]string) {
	s.endStream()
}

func (s *stream) endStream() {

	if s.encodedHeaders != nil {
		s.connection.activeStreams[s.streamId].connection.connection.Write(s.encodedHeaders)
		if s.encodedData != nil {
			s.connection.activeStreams[s.streamId].connection.connection.Write(s.encodedData)
		} else {
			log.DefaultLogger.Println("Response Body is void...")
		}

	} else {
		log.DefaultLogger.Println("Response Headers is void...")
	}

	if s.direction == 1 {
		delete(s.connection.activeStreams, s.streamId)
	}
}

func (s *stream) GetStream() types.Stream {
	return s
}