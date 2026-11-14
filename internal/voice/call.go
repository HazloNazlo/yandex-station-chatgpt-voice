package voice

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/pion/logging"
	"github.com/pion/sctp"
	"github.com/pion/webrtc/v4"
)

const signalingTimeout = 30 * time.Second

const maxRTCPPacketBytes = 1500

const signalingURL = "https://chatgpt.com/realtime/wm?dcid=0"
const maxAnswerBytes = 1 << 20
const maxSCTPReceiveBufferBytes = 1 << 20

var errRemoteClosed = errors.New("remote voice connection closed")
var errProtocolHangup = errors.New("voice protocol hangup")

type peer struct {
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	dcMu      sync.Mutex
	mic       *webrtc.TrackLocalStaticSample
	ctx       context.Context
	cancel    context.CancelCauseFunc
	ready     chan struct{}
	tracks    chan *webrtc.TrackRemote
	trackSeen atomic.Bool
	wg        sync.WaitGroup
	close     func() error
	closing   atomic.Bool
	report    *log.Logger
	failureMu sync.Mutex
	failure   error
	abort     context.CancelCauseFunc
	callsMu      sync.Mutex
	seenCalls    map[string]bool
	audio        *audio
	userMu       sync.Mutex
	userTextBuf  strings.Builder
	isUserMsg    bool
	lastActivity atomic.Int64
}

//nolint:funlen,gocognit // Callback closures share peer teardown and cancellation state; keep their registration together.
func newPeer(
	ctx context.Context,
	settings webrtc.SettingEngine,
	servers []webrtc.ICEServer,
	report *log.Logger,
	abort context.CancelCauseFunc,
) (*peer, error) {
	settings.SetSCTPMaxMessageSize(maxMessageBytes)
	settings.SetSCTPMaxReceiveBufferSize(maxSCTPReceiveBufferBytes)
	settings.LoggerFactory = &logging.DefaultLoggerFactory{
		Writer:          io.Discard,
		DefaultLogLevel: logging.LogLevelDisabled,
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	ctx, cancel := context.WithCancelCause(ctx)
	p := &peer{
		report:    report,
		ctx:       ctx,
		cancel:    cancel,
		ready:     make(chan struct{}),
		tracks:    make(chan *webrtc.TrackRemote, 1),
		abort:     abort,
		seenCalls: make(map[string]bool),
	}
	var err error
	p.pc, err = api.NewPeerConnection(
		webrtc.Configuration{ICEServers: servers, BundlePolicy: webrtc.BundlePolicyMaxBundle},
	)
	if err != nil {
		cancel(err)
		return nil, errors.New("cannot create WebRTC peer")
	}
	p.close = sync.OnceValue(func() error {
		p.closing.Store(true)
		p.cancel(context.Canceled)
		// Peer closure tears down SCTP before joining the DataChannel read loop.
		pcErr := p.pc.GracefulClose()
		p.wg.Wait()
		if pcErr != nil {
			return fmt.Errorf("%w: WebRTC did not close cleanly", errCleanup)
		}
		return nil
	})
	fail := func(message string) (*peer, error) { return nil, errors.Join(errors.New(message), p.close()) }
	p.mic, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   playbackSampleRate,
			Channels:    stereoChannels,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		"microphone",
		"voice",
	)
	if err != nil {
		return fail("cannot create Opus track")
	}
	tx, err := p.pc.AddTransceiverFromTrack(
		p.mic,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv},
	)
	if err != nil {
		return fail("cannot attach Opus track")
	}
	codecs := tx.Sender().GetParameters().Codecs
	var opusCodecs []webrtc.RTPCodecParameters
	for _, c := range codecs {
		if c.MimeType == webrtc.MimeTypeOpus && c.ClockRate == 48000 && c.Channels == 2 {
			opusCodecs = append(opusCodecs, c)
		}
	}
	if len(opusCodecs) == 0 || tx.SetCodecPreferences(opusCodecs) != nil {
		return fail("cannot select Opus codec")
	}
	p.dc, err = p.pc.CreateDataChannel(
		"",
		&webrtc.DataChannelInit{Negotiated: new(true), Ordered: new(true), ID: new(uint16(0))},
	)
	if err != nil {
		return fail("cannot create voice control channel")
	}
	p.dc.OnOpen(sync.OnceFunc(func() { close(p.ready) }))
	p.dc.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString {
			p.fail(errors.New("unexpected binary voice control message"))
			return
		}
		p.handleMessage(message.Data)
	})
	p.dc.OnClose(func() { p.cancel(errRemoteClosed) })
	p.dc.OnError(p.channelError)
	var disconnected atomic.Bool
	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected:
			disconnected.Store(true)
			p.report.Print("Voice transport disconnected; waiting for native recovery.")
		case webrtc.PeerConnectionStateConnected:
			if disconnected.Swap(false) {
				p.report.Print("Voice transport recovered.")
			}
		case webrtc.PeerConnectionStateFailed:
			p.fail(errors.New("WebRTC connection lost"))
		case webrtc.PeerConnectionStateClosed:
			p.cancel(errRemoteClosed)
		case webrtc.PeerConnectionStateUnknown, webrtc.PeerConnectionStateNew, webrtc.PeerConnectionStateConnecting:
		}
	})
	p.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if p.ctx.Err() != nil {
			return
		}
		if track.Kind() != webrtc.RTPCodecTypeAudio || !p.trackSeen.CompareAndSwap(false, true) {
			p.cancel(errors.New("unexpected additional remote media track"))
			return
		}
		select {
		case p.tracks <- track:
		case <-p.ctx.Done():
		default:
			p.cancel(errors.New("remote track mailbox overflow"))
		}
	})
	p.wg.Go(func() {
		buffer := make([]byte, maxRTCPPacketBytes)
		for {
			if _, _, readErr := tx.Sender().Read(buffer); readErr != nil {
				if p.ctx.Err() == nil && !closedTransport(readErr) {
					p.fail(errors.New("microphone RTCP transport failed"))
				}
				return
			}
		}
	})
	return p, nil
}

func closedTransport(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled)
}

func (p *peer) fail(err error) {
	p.failureMu.Lock()
	if p.failure == nil {
		p.failure = err
	}
	p.failureMu.Unlock()
	if p.abort != nil {
		p.abort(err)
	}
	p.cancel(err)
}

func (p *peer) channelError(err error) {
	// Pion's Stop aborts SCTP; its final stream reset can race association closure.
	if closedTransport(err) || errors.Is(err, sctp.ErrUserInitiatedAbort) ||
		errors.Is(err, sctp.ErrResetPacketInStateNotExist) {
		return
	}
	// SCTP Stop forces a DTLS read deadline to unblock local teardown.
	if p.closing.Load() && errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// Pion can dispatch this before shutdown and run it after OnClose or GracefulClose.
	p.fail(errors.New("voice control channel failed"))
	p.report.Print("Voice control channel failed.")
}

func (p *peer) outcome(result error) error {
	p.failureMu.Lock()
	defer p.failureMu.Unlock()
	//nolint:errorlint // Only the exact sentinel is benign; wrapped or joined failures must remain failures.
	if result == errProtocolHangup {
		result = nil
	}
	if p.failure != nil && !errors.Is(result, p.failure) {
		result = errors.Join(result, p.failure)
	}
	return result
}

func (p *peer) sendText(text string) error {
	p.dcMu.Lock()
	defer p.dcMu.Unlock()
	if p.dc == nil {
		return errors.New("voice control channel is nil")
	}
	return p.dc.SendText(text)
}

func (p *peer) sendRelayText(text string) error {
	msgID := strings.ToLower(uuid.New().String())
	now := float64(time.Now().UnixNano()) / 1e9

	innerMsg := map[string]any{
		"type": "relay_message",
		"payload": map[string]any{
			"type": "relay_message",
			"message": map[string]any{
				"id":          msgID,
				"author":      map[string]any{"role": "user"},
				"create_time": now,
				"content": map[string]any{
					"content_type": "text",
					"parts":        []string{text},
				},
				"metadata": map[string]any{
					"serialization_metadata": map[string]any{
						"custom_symbol_offsets": []any{},
					},
				},
				"clientMetadata": map[string]any{
					"isOptimistic": true,
				},
			},
		},
	}
	innerBytes, err := json.Marshal(innerMsg)
	if err != nil {
		return err
	}

	outerMsg := map[string]any{
		"type": "data_message",
		"data": string(innerBytes),
	}
	outerBytes, err := json.Marshal(outerMsg)
	if err != nil {
		return err
	}

	return p.sendText(string(outerBytes))
}

func (p *peer) handleMessage(data []byte) {
	p.lastActivity.Store(time.Now().Unix())
	//nolint:errorlint // Only the exact sentinel is benign; wrapped or joined failures must remain failures.
	if p.ctx.Err() != nil && context.Cause(p.ctx) != errProtocolHangup {
		return
	}

	unwrapped, eventType := extractWirePayload(data)
	if eventType != "" && eventType != "chat_message_delta" {
		p.report.Printf("DC recv event [%s]: %s", eventType, string(unwrapped))
	}

	switch eventType {
	case "chat_message_delta":
		var cmd struct {
			Delta struct {
				C int            `json:"c"`
				P string         `json:"p"`
				O string         `json:"o"`
				V jsontext.Value `json:"v"`
			} `json:"delta"`
		}
		if json.Unmarshal(unwrapped, &cmd) == nil {
			p.userMu.Lock()
			finished := false

			if cmd.Delta.O == "add" {
				var addMsg struct {
					Message struct {
						ID     string `json:"id"`
						Author struct {
							Role string `json:"role"`
						} `json:"author"`
						Content struct {
							Parts []struct {
								Text string `json:"text"`
							} `json:"parts"`
						} `json:"content"`
					} `json:"message"`
				}
				if json.Unmarshal(cmd.Delta.V, &addMsg) == nil {
					if addMsg.Message.Author.Role == "user" {
						p.isUserMsg = true
						p.userTextBuf.Reset()
						for _, pt := range addMsg.Message.Content.Parts {
							p.userTextBuf.WriteString(pt.Text)
						}
					} else {
						// Assistant message starting -> previous user turn has completed
						if p.isUserMsg {
							finished = true
						}
					}
				}
			}

			// Handle patches array if present in cmd.Delta.V (whether cmd.Delta.O is "patch" or empty)
			if len(cmd.Delta.V) > 0 && cmd.Delta.V[0] == '[' {
				var patches []struct {
					P string         `json:"p"`
					O string         `json:"o"`
					V jsontext.Value `json:"v"`
				}
				if json.Unmarshal(cmd.Delta.V, &patches) == nil {
					for _, pt := range patches {
						if pt.O == "append" && strings.HasPrefix(pt.P, "/message/content/parts/") {
							if p.isUserMsg {
								var textPart string
								if json.Unmarshal(pt.V, &textPart) == nil {
									p.userTextBuf.WriteString(textPart)
								}
							}
						}
						if pt.P == "/message/status" {
							var status string
							if json.Unmarshal(pt.V, &status) == nil && status == "finished_successfully" {
								if p.isUserMsg {
									finished = true
								}
							}
						}
					}
				}
			}

			// Handle single patch status on cmd.Delta
			if cmd.Delta.P == "/message/status" {
				var val string
				if json.Unmarshal(cmd.Delta.V, &val) == nil && val == "finished_successfully" {
					if p.isUserMsg {
						finished = true
					}
				}
			}

			if finished && p.isUserMsg {
				fullText := strings.TrimSpace(p.userTextBuf.String())
				p.userTextBuf.Reset()
				p.isUserMsg = false
				p.userMu.Unlock()

				if fullText != "" {
					p.report.Printf("User speech recognized: %q", fullText)
					p.lastActivity.Store(time.Now().Unix())

					// 1. Hangup check: "пока", "до свидания", "отбой", "стоп", "хватит", "заверши сессию"
					if isHangup, _ := parseHangupCommand(fullText); isHangup {
						p.report.Printf("Hangup command triggered by speech: %q", fullText)
						p.wg.Go(func() {
							time.Sleep(1200 * time.Millisecond)
							p.cancel(errProtocolHangup)
						})
						return
					}

					// 2. Voice change check: "смени голос", "поставь женский голос", "какие есть голоса"
					if isVoiceCmd, newVoice, msg := parseVoiceCommand(fullText); isVoiceCmd {
						p.report.Printf("Voice command triggered: %q -> %s", fullText, newVoice)
						p.wg.Go(func() {
							if newVoice != "" {
								if err := saveVoice(newVoice); err == nil {
									p.report.Printf("Saved new voice preference: %s", newVoice)
								}
							}
							if p.audio != nil {
								p.audio.flushPlayback()
							}
							if msg != "" {
								_ = p.sendRelayText(fmt.Sprintf("Скажи пользователю строго следующее: %s", msg))
							}
							if newVoice != "" {
								// Wait 3.5s for confirmation to be spoken, then hang up so next "Яндекс" starts with new voice
								time.Sleep(3500 * time.Millisecond)
								p.cancel(errProtocolHangup)
							}
						})
						return
					}

					// 3. Volume command check
					if isVol, newVol, delta := parseVolumeCommand(fullText); isVol {
						if newVol >= 0 {
							if actual, err := setStationVolume(newVol); err == nil {
								p.report.Printf("Volume command executed: Station volume set to %d%%", actual)
							} else {
								p.report.Printf("Volume error: %v", err)
							}
						} else if delta != 0 {
							if actual, err := adjustStationVolume(delta); err == nil {
								p.report.Printf("Volume command executed: Station volume adjusted by %d to %d%%", delta, actual)
							} else {
								p.report.Printf("Volume error: %v", err)
							}
						}
					}

					// 4. Web search check
					if needSearch, query := detectSearchQuery(fullText); needSearch {
						if p.audio != nil {
							p.audio.flushPlayback()
						}
						p.wg.Go(func() {
							p.report.Printf("Web search triggered for: %q", query)
							searchCtx, cancel := context.WithTimeout(p.ctx, 4*time.Second)
							defer cancel()
							results, err := searchWeb(searchCtx, query)
							if err != nil {
								p.report.Printf("Search error: %v", err)
								return
							}
							p.report.Printf("Search completed. Sending relay text to ChatGPT...")
							relayPrompt := fmt.Sprintf("[Данные из интернета]: Ты голосовой ассистент на умной колонке Яндекс Станция. У тебя есть постоянный доступ в интернет. Используй эти проверенные данные, чтобы емко и четко ответить пользователю на его вопрос %q:\n%s", query, results)
							if p.audio != nil {
								p.audio.flushPlayback()
							}
							if err := p.sendRelayText(relayPrompt); err != nil {
								p.report.Printf("Failed to send relay search: %v", err)
							} else {
								p.report.Print("Relay search info delivered to ChatGPT.")
							}
						})
					}
				}
			} else {
				p.userMu.Unlock()
			}
		}

	case "input_audio_buffer.speech_started":
		p.lastActivity.Store(time.Now().Unix())
		if p.audio != nil {
			p.audio.flushPlayback()
		}
	}

	calls := parseToolCalls(data)
	for _, call := range calls {
		p.callsMu.Lock()
		alreadySeen := p.seenCalls[call.CallID]
		if !alreadySeen {
			p.seenCalls[call.CallID] = true
		}
		p.callsMu.Unlock()

		if alreadySeen {
			continue
		}

		callID := call.CallID
		name := call.Name
		args := call.Arguments

		p.wg.Go(func() {
			p.report.Printf("Executing tool %q (call_id: %s) with args: %s", name, callID, args)
			var output string
			var execErr error

			switch name {
			case "set_volume":
				output, execErr = handleSetVolume(args)
			case "set_voice":
				output, execErr = handleSetVoice(args)
				p.wg.Go(func() {
					time.Sleep(2500 * time.Millisecond)
					p.cancel(errProtocolHangup)
				})
			case "search_web":
				searchCtx, searchCancel := context.WithTimeout(p.ctx, 8*time.Second)
				defer searchCancel()
				output, execErr = handleSearchWeb(searchCtx, args)
			case "hang_up", "end_session":
				output = `{"status":"hanging_up"}`
				p.wg.Go(func() {
					time.Sleep(1200 * time.Millisecond)
					p.cancel(errProtocolHangup)
				})
			default:
				output = fmt.Sprintf(`{"error": "unknown tool %s"}`, name)
			}

			if execErr != nil {
				p.report.Printf("Tool %q error: %v", name, execErr)
				output = fmt.Sprintf(`{"error": %q}`, execErr.Error())
			} else {
				p.report.Printf("Tool %q completed successfully: %s", name, output)
			}

			itemMsg := map[string]any{
				"type": "conversation.item.create",
				"item": map[string]any{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  output,
				},
			}
			itemBytes, _ := json.Marshal(itemMsg)
			if err := p.sendText(string(itemBytes)); err != nil {
				p.report.Printf("Failed to send function_call_output: %v", err)
				return
			}

			respMsg := map[string]any{
				"type": "response.create",
			}
			respBytes, _ := json.Marshal(respMsg)
			if err := p.sendText(string(respBytes)); err != nil {
				p.report.Printf("Failed to send response.create: %v", err)
				return
			}
			p.report.Printf("Triggered ChatGPT response after tool %q.", name)
		})
	}

	ev, err := decodeEvent(data)
	switch {
	case err != nil:
		p.fail(err)
	case ev.err != nil:
		p.fail(ev.err)
	case ev.ended:
		p.cancel(errProtocolHangup)
	}
}

func (p *peer) offer(ctx context.Context) (string, error) {
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", errors.New("cannot create voice offer")
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if p.pc.SetLocalDescription(offer) != nil {
		return "", errors.New("cannot set voice offer")
	}
	gatherCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	select {
	case <-gatherCtx.Done():
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		return "", errors.New("ICE gathering did not complete before timeout")
	case <-p.ctx.Done():
		return "", context.Cause(p.ctx)
	case <-gathered:
	}
	if gatherCtx.Err() != nil {
		return "", errors.New("ICE gathering did not complete before cancellation or timeout")
	}
	description := p.pc.LocalDescription()
	if description == nil || p.pc.ICEGatheringState() != webrtc.ICEGatheringStateComplete {
		return "", errors.New("ICE offer is incomplete")
	}
	return description.SDP, nil
}

func (p *peer) accept(answer string) error {
	if p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}) != nil {
		return errors.New("ChatGPT SDP answer was not accepted by the native peer")
	}
	return nil
}

//nolint:gochecknoglobals // Offline session tests replace this HTTP-client boundary while exercising fixed-origin requests.
var callClient = signalingClient

func signalingClient() *http.Client {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("default HTTP transport must be *http.Transport")
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	transport.MaxResponseHeaderBytes = maxSessionResponseBytes
	return &http.Client{
		Transport:     transport,
		Timeout:       signalingTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

//nolint:funlen // Keep request validation, response bounds, and sanitized failures together.
func exchangeOffer(
	ctx context.Context,
	client *http.Client,
	auth credential,
	offer string,
	session []byte,
) (string, error) {
	if !validCredential(auth) {
		return "", errors.New("invalid web credential")
	}
	if len(offer) > maxAnswerBytes || !strings.HasPrefix(offer, "v=0") {
		return "", errors.New("invalid local SDP offer")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("sdp", offer); err != nil {
		return "", errors.New("cannot encode offer")
	}
	if err := form.WriteField("session", string(session)); err != nil {
		return "", errors.New("cannot encode voice session")
	}
	if form.Close() != nil {
		return "", errors.New("cannot finish offer")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signalingURL, &body)
	if err != nil {
		return "", errors.New("cannot prepare signaling request")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", "yandex-station-chatgpt-voice/0.1")
	req.Header.Set("Oai-Device-Id", auth.DeviceID)
	req.Header.Set("Oai-Language", "en-US")
	req.Header.Set("Oai-Client-Version", "prod-fb4a8a2a751dfec391053cfd7b01c52699ccf78c")
	req.Header.Set("Oai-Client-Build-Number", "8370486")
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := safeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		return "", errors.New("voice signaling HTTPS request failed; check network, TLS, and service availability")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return "", errors.New("ChatGPT rejected the web session (401); run make login on the computer")
	case http.StatusForbidden:
		return "", errors.New(
			"ChatGPT rejected voice access (403); account entitlement or edge restrictions may prevent this native client",
		)
	case http.StatusOK, http.StatusCreated:
	default:
		return "", fmt.Errorf("ChatGPT voice signaling returned HTTP %d", response.StatusCode)
	}
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		return "", errors.New("cannot read voice SDP answer")
	}
	if len(answer) > maxAnswerBytes {
		return "", errors.New("voice SDP answer exceeds 1 MiB")
	}
	sdp := strings.TrimSpace(string(answer))
	if !strings.HasPrefix(sdp, "v=0\r\n") && !strings.HasPrefix(sdp, "v=0\n") {
		return "", errors.New("ChatGPT did not return an SDP answer")
	}
	return sdp + "\r\n", nil
}

//nolint:cyclop,errorlint,funlen,gocognit,gocyclo // Keep ordered media teardown and exact sentinel identity; aggregates remain failures.
func runCall(e *stationEpoch, sessionFile string, capture *stationCapture, settings sessionSettings) (result error) {
	defer func() {
		switch result {
		case nil, errAlicePriority, context.Canceled:
			return
		case context.DeadlineExceeded:
			if e.ctx.Err() == context.DeadlineExceeded {
				return
			}
		}
		if !errors.Is(result, errStationAudio) {
			result = voiceCallError{result}
		}
	}()
	if !e.allowed() {
		return context.Cause(e.ctx)
	}
	client := callClient()
	defer client.CloseIdleConnections()
	renewCtx, stopRenew := context.WithTimeoutCause(e.ctx, renewTimeout, errRenewTimeout)
	auth, err := sessionForCall(renewCtx, sessionFile, client, e.report)
	stopRenew()
	if err != nil {
		return err
	}
	ctx := e.ctx
	var workers sync.WaitGroup
	callStart := time.Now()
	settingsEngine := webrtc.SettingEngine{}
	settingsEngine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settingsEngine.SetInterfaceFilter(func(name string) bool {
		return name == "wlan0" || strings.HasPrefix(name, "wl")
	})
	p, err := newPeer(
		ctx,
		settingsEngine,
		[]webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
		e.report,
		e.revoke,
	)
	if err != nil {
		return err
	}
	ctx = p.ctx
	playbackOpened := false
	complete := func(cause error) error {
		if cause != errProtocolHangup {
			return cause
		}
		capture.beginDraining()
		workers.Wait()
		cleanup := p.close()
		if failure := p.outcome(errProtocolHangup); failure != nil {
			return errors.Join(failure, cleanup)
		}
		if cleanup != nil {
			return cleanup
		}
		if playbackOpened {
			e.report.Print("Voice call ended by ChatGPT; finishing received audio.")
			if finishErr := e.output.finish(e.ctx); finishErr != nil {
				return finishErr
			}
		}
		return errProtocolHangup
	}
	shutdown := sync.OnceValue(func() error {
		e.revoke(context.Canceled)
		return errors.Join(e.output.close(), p.close())
	})
	stopShutdown := context.AfterFunc(e.ctx, func() { _ = shutdown() })
	defer func() {
		e.revoke(context.Canceled)
		cleanupErr := shutdown()
		stopShutdown()
		workers.Wait()
		result = p.outcome(result)
		if cleanupErr != nil {
			result = errors.Join(result, cleanupErr)
		}
	}()
	stopPeerCancel := context.AfterFunc(p.ctx, func() {
		if cause := context.Cause(p.ctx); cause != errProtocolHangup {
			e.revoke(cause)
		}
	})
	defer stopPeerCancel()
	setupCtx, finishSetup := context.WithTimeout(ctx, setupTimeout)
	defer finishSetup()
	stopSetupTimeout := context.AfterFunc(setupCtx, func() {
		if errors.Is(setupCtx.Err(), context.DeadlineExceeded) {
			e.revoke(errors.New("voice connection setup timed out"))
		}
	})
	defer stopSetupTimeout()
	offer, err := p.offer(setupCtx)
	if err != nil {
		return complete(err)
	}
	e.report.Printf("ICE offer ready in %v", time.Since(callStart))
	session, err := encodeSession(time.Now(), settings)
	if err != nil {
		return errors.New("cannot encode voice session")
	}
	sigStart := time.Now()
	answer, err := exchangeOffer(setupCtx, client, auth, offer, session)
	if err != nil {
		return complete(err)
	}
	e.report.Printf("Signaling exchange in %v", time.Since(sigStart))
	if ctx.Err() != nil {
		return complete(context.Cause(ctx))
	}
	if err = p.accept(answer); err != nil {
		return err
	}
	ready := false
	waitStart := time.Now()
	for !ready {
		select {
		case <-setupCtx.Done():
			if ctx.Err() != nil {
				return complete(context.Cause(ctx))
			}
			return errors.New("voice connection setup timed out")
		case <-p.ready:
			ready = true
		}
	}
	e.report.Printf("WebRTC DataChannel ready in %v (total setup: %v)", time.Since(waitStart), time.Since(callStart))

	if ctx.Err() != nil {
		return complete(context.Cause(ctx))
	}
	if setupCtx.Err() != nil {
		return errors.New("voice setup ended before media became ready")
	}
	p.audio = e.output
	if err = e.output.open(); err != nil {
		return err
	}
	playbackOpened = true
	if ctx.Err() != nil {
		return complete(context.Cause(ctx))
	}
	if setupCtx.Err() != nil {
		return errors.New("voice setup ended while opening playback")
	}
	if err = playCue(setupCtx, readyCueFrequency); err != nil {
		return complete(err)
	}
	if err = capture.beginStreaming(setupCtx); err != nil {
		return complete(err)
	}
	finishSetup()
	e.report.Print("Voice media ready.")
	p.lastActivity.Store(time.Now().Unix())
	workers.Go(func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if p.audio != nil && p.audio.isPlaying() {
					p.lastActivity.Store(time.Now().Unix())
					continue
				}
				last := p.lastActivity.Load()
				if last > 0 && time.Now().Unix()-last > 90 {
					p.report.Print("Session silence timeout (90s inactivity); ending call gracefully.")
					p.cancel(errProtocolHangup)
					return
				}
			}
		}
	})
	workers.Go(func() {
		if sendErr := e.output.send(ctx, p.mic, capture.input); sendErr != nil {
			e.revoke(sendErr)
		}
	})
	for {
		select {
		case <-ctx.Done():
			return complete(context.Cause(ctx))
		case track := <-p.tracks:
			workers.Go(func() {
				if receiveErr := e.output.receive(ctx, track); receiveErr != nil {
					e.revoke(receiveErr)
				}
			})
		}
	}
}
