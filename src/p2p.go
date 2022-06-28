package ggthx

import (
	"log"
	"math"
	"time"
)

const RECOMMENDATION_INTERVAL int = 240
const DEFAULT_DISCONNECT_TIMEOUT int = 5000
const DEFAULT_DISCONNECT_NOTIFY_START int = 750

type Peer2PeerBackend struct {
	callbacks     GGTHXSessionCallbacks
	poll          Poll
	sync          Sync
	udp           Udp
	endpoints     []UdpProtocol
	spectators    []UdpProtocol
	numSpectators int
	inputSize     int

	synchronizing        bool
	numPlayers           int
	nextRecommendedSleep int

	nextSpectatorFrame    int
	disconnectTimeout     int
	disconnectNotifyStart int

	localConnectStatus []UdpConnectStatus
}

func NewPeer2PeerBackend(cb *GGTHXSessionCallbacks, gameName string,
	localPort int, numPlayers int, inputSize int) Peer2PeerBackend {
	p := Peer2PeerBackend{}
	p.numPlayers = numPlayers
	p.inputSize = inputSize
	p.callbacks = *cb
	p.synchronizing = true
	p.disconnectTimeout = DEFAULT_DISCONNECT_TIMEOUT
	p.disconnectNotifyStart = DEFAULT_DISCONNECT_NOTIFY_START
	p.poll = NewPoll()
	p.udp = NewUdp("127.0.0.1", localPort, &p.poll, &p)

	p.localConnectStatus = make([]UdpConnectStatus, UDP_MSG_MAX_PLAYERS)
	for i := 0; i < len(p.localConnectStatus); i++ {
		p.localConnectStatus[i].LastFrame = -1
	}
	var config SyncConfig
	config.numPlayers = numPlayers
	config.inputSize = inputSize
	config.callbacks = p.callbacks
	config.numPredictionFrames = MAX_PREDICTION_FRAMES
	p.sync = NewSync(p.localConnectStatus, &config)
	p.endpoints = make([]UdpProtocol, numPlayers)
	p.callbacks.BeginGame(gameName)
	go p.udp.Read()

	return p
}

func (p *Peer2PeerBackend) Close() GGTHXErrorCode {
	for _, e := range p.endpoints {
		e.Close()
	}
	for _, s := range p.spectators {
		s.Close()
	}
	return GGTHX_OK
}
func (p *Peer2PeerBackend) DoPoll(timeout int) GGTHXErrorCode {
	if !p.sync.InRollback() {
		p.poll.Pump(0)

		p.PollUdpProtocolEvents()

		if !p.synchronizing {
			p.sync.CheckSimulation(timeout)

			// notify all of our endpoints of their local frame number for their
			// next connection quality report
			currentFrame := p.sync.GetFrameCount()
			for i := 0; i < p.numPlayers; i++ {
				p.endpoints[i].SetLocalFrameNumber(currentFrame)
			}
			var totalMinConfirmed int
			if p.numPlayers <= 2 {
				totalMinConfirmed = p.Poll2Players(currentFrame)
			} else {
				totalMinConfirmed = p.PollNPlayers(currentFrame)
			}

			log.Printf("last confirmed frame in p2p backend is %d.\n", totalMinConfirmed)
			if totalMinConfirmed >= 0 {
				Assert(totalMinConfirmed != math.MaxInt)
				if p.numSpectators > 0 {
					for p.nextSpectatorFrame <= totalMinConfirmed {
						log.Printf("pushing frame %d to spectators.\n", p.nextSpectatorFrame)

						var input GameInput
						input.Frame = p.nextSpectatorFrame
						input.Size = p.inputSize * p.numPlayers
						input.Inputs, _ = p.sync.GetConfirmedInputs(p.nextSpectatorFrame)
						for i := 0; i < p.numSpectators; i++ {
							p.spectators[i].SendInput(&input)
						}
						p.nextSpectatorFrame++
					}
				}
				log.Printf("setting confirmed frame in sync to %d.\n", totalMinConfirmed)
				p.sync.SetLastConfirmedFrame(totalMinConfirmed)
			}

			// send timesync notifications if now is the proper time
			if currentFrame > p.nextRecommendedSleep {
				interval := 0
				for i := 0; i < p.numPlayers; i++ {
					interval = Max(interval, p.endpoints[i].RecommendFrameDelay())
				}

				if interval > 0 {
					var info GGTHXEvent
					info.Code = GGTHX_EVENTCODE_TIMESYNC
					info.framesAhead = interval
					p.callbacks.OnEvent(&info)
					p.nextRecommendedSleep = currentFrame + RECOMMENDATION_INTERVAL
				}
			}
			// because GGPO had this
			if timeout > 0 {
				time.Sleep(time.Second)
			}
		}
	}
	return GGTHX_OK
}

// Checks each endpoint to see if it's running (i.e we haven't called Synchronize or Disconnect
// on it lately )
// Again check if it's disconnected (GetPeerConnectionStatus which would check if the user has
// sent a disconnect message.
// Try to get the minimum last confirmed frame across all the inputs
// If by chance the user sent a disconnect request and we haven't disconnected them yet,
// We disconnect them.
// The value returned by this function (the total minimum confirmed frame across all the inputs)
// is used in DoPoll to set the last confirmed frame in the sync backend, which tells
// the input queue to discard the frame before that.
func (p *Peer2PeerBackend) Poll2Players(currentFrame int) int {
	totalMinConfirmed := math.MaxInt
	for i := 0; i < p.numPlayers; i++ {
		queueConnected := true
		if p.endpoints[i].IsRunning() {
			var ignore int
			queueConnected = p.endpoints[i].GetPeerConnectStatus(i, &ignore)
		}
		if p.localConnectStatus[i].Disconnected == 0 {
			totalMinConfirmed = Min(p.localConnectStatus[i].LastFrame, totalMinConfirmed)
		}
		log.Printf("  local endp: connected = %t, last_received = %d, total_min_confirmed = %d.\n",
			p.localConnectStatus[i].Disconnected == 0, p.localConnectStatus[i].LastFrame, totalMinConfirmed)
		if !queueConnected && p.localConnectStatus[i].Disconnected == 0 {
			log.Printf("disconnecting i %d by remote request.\n", i)
			p.DisconnectPlayerQueue(i, totalMinConfirmed)
		}
		log.Printf("  total_min_confirmed = %d.\n", totalMinConfirmed)
	}
	return totalMinConfirmed
}

// Just for parity with GGPO. Don't care to actually use this.
func (p *Peer2PeerBackend) PollNPlayers(currentFrame int) int {
	var i, queue, lastRecieved int

	totalMinConfirmed := math.MaxInt
	for queue = 0; queue < p.numPlayers; queue++ {
		queueConnected := true
		queueMinConfirmed := math.MaxInt
		log.Printf("considering queue %d.\n", queue)
		for i = 0; i < p.numPlayers; i++ {
			// we're going to do a lot of logic here in consideration of endpoint i.
			// keep accumulating the minimum confirmed point for all n*n packets and
			// throw away the rest. -pond3r
			if p.endpoints[i].IsRunning() {
				connected := p.endpoints[i].GetPeerConnectStatus(queue, &lastRecieved)
				queueMinConfirmed = Min(lastRecieved, queueMinConfirmed)
				log.Printf("  endpoint %d: connected = %t, last_received = %d, queue_min_confirmed = %d.\n",
					i, connected, lastRecieved, queueMinConfirmed)
			} else {
				log.Printf("  endpoint %d: ignoring... not running.\n", i)
			}
		}
		// merge in our local status only if we're still connected!
		if p.localConnectStatus[queue].Disconnected == 0 {
			queueMinConfirmed = Min(p.localConnectStatus[queue].LastFrame, queueMinConfirmed)
		}
		log.Printf("  local endp: connected = %t, last_received = %d, queue_min_confirmed = %d.\n",
			p.localConnectStatus[queue].Disconnected == 0, p.localConnectStatus[queue].LastFrame, queueMinConfirmed)

		if queueConnected {
			totalMinConfirmed = Min(queueMinConfirmed, totalMinConfirmed)
		} else {
			// check to see if this disconnect notification is further back than we've been before.  If
			// so, we need to re-adjust.  This can happen when we detect our own disconnect at frame n
			// and later receive a disconnect notification for frame n-1.
			if p.localConnectStatus[queue].Disconnected == 0 || p.localConnectStatus[queue].LastFrame > queueMinConfirmed {
				log.Printf("disconnecting queue %d by remote request.\n", queue)
				p.DisconnectPlayerQueue(queue, queueMinConfirmed)
			}
		}
		log.Printf("  total_min_confirmed = %d.\n", totalMinConfirmed)
	}
	return totalMinConfirmed
}

/*
	Giving each spectator and remote player their own UDP object (which GGPO didn't do)
	a copy of the poll, a copy of our localConnectStatus (might want to send a pointer?)
	Setting the default disconnect timeout and disconnect notify
	And calling the synchronize method, which sends a sync request to that endpoint.
*/
func (p *Peer2PeerBackend) AddRemotePlayer(ip string, port int, queue int) {
	p.synchronizing = true

	p.endpoints[queue] = NewUdpProtocol(&p.udp, &p.poll, queue, ip, port, p.localConnectStatus)
	// have to reqgister the loop from here or else the Poll won't see changed state
	// that we've initiated.
	p.poll.RegisterLoop(&(p.endpoints[queue]), nil)

	// actually this DoPoll wouldn't run at all if it wasn't called from here.
	//p.poll.RegisterLoop(&udp, nil)
	p.endpoints[queue].SetDisconnectTimeout(p.disconnectTimeout)
	p.endpoints[queue].SetDisconnectNotifyStart(p.disconnectNotifyStart)
	p.endpoints[queue].Synchronize()
}

func (p *Peer2PeerBackend) AddSpectator(ip string, port int) GGTHXErrorCode {
	if p.numSpectators == GGTHX_MAX_SPECTATORS {
		return GGTHX_ERRORCODE_TOO_MANY_SPECTATORS
	}
	// Currently, we can only add spectators before the game starts.
	if !p.synchronizing {
		return GGTHX_ERRORCODE_INVALID_REQUEST
	}
	queue := p.numSpectators
	p.numSpectators++
	udp := NewUdp(ip, port, &p.poll, p)
	p.spectators[queue] = NewUdpProtocol(&udp, &p.poll, queue+1000, ip, port, p.localConnectStatus)
	p.poll.RegisterLoop(&(p.spectators[queue]), nil)
	p.spectators[queue].SetDisconnectTimeout(p.disconnectTimeout)
	p.spectators[queue].SetDisconnectNotifyStart(p.disconnectNotifyStart)
	p.spectators[queue].Synchronize()

	return GGTHX_OK
}

// Adds player or spectator
// Maps to top level API function
func (p *Peer2PeerBackend) AddPlayer(player *GGTHXPlayer, handle *GGTHXPlayerHandle) GGTHXErrorCode {
	if player.PlayerType == GGTHX_PLAYERTYPE_SPECTATOR {
		return p.AddSpectator(player.Remote.IpAdress, player.Remote.Port)
	}

	queue := player.PlayerNum - 1
	if player.PlayerNum < 1 || player.PlayerNum > p.numPlayers {
		return GGTHX_ERRORCODE_PLAYER_OUT_OF_RANGE
	}
	*handle = p.QueueToPlayerHandle(queue)

	if player.PlayerType == GGTHX_PLAYERTYPE_REMOTE {
		p.AddRemotePlayer(player.Remote.IpAdress, player.Remote.Port, queue)
	}

	return GGTHX_OK
}

// Sends input to the synchronization layer and to all the endpoints.
// Which adds it to those respective queues (inputQueue for synchronization layer, pendingOutput
// for endpoint)
// Maps to top level API function.
func (p *Peer2PeerBackend) AddLocalInput(player GGTHXPlayerHandle, values []byte, size int) GGTHXErrorCode {
	var queue int
	var input GameInput

	if p.sync.InRollback() {
		return GGTHX_ERRORCODE_IN_ROLLBACK
	}
	if p.synchronizing {
		return GGTHX_ERRORCODE_NOT_SYNCHRONIZED
	}

	input = NewGameInput(-1, values, size)
	// Feed the input for the current frame into the synchronization layer.
	if !p.sync.AddLocalInput(queue, &input) {
		return GGTHX_ERRORCODE_PREDICTION_THRESHOLD
	}

	if input.Frame != NullFrame {
		// Update the local connect status state to indicate that we've got a
		// confirmed local frame for this player.  this must come first so it
		// gets incorporated into the next packet we send.
		// - pond3r

		log.Printf("setting local connect status for local queue %d to %d", queue, input.Frame)
		p.localConnectStatus[queue].LastFrame = input.Frame

		// Send the input to all the remote players.
		for i := 0; i < p.numPlayers; i++ {
			if p.endpoints[i].IsInitialized() {
				p.endpoints[i].SendInput(&input)
			}
		}
	}

	return GGTHX_OK
}

// Maps to top level API function
// Returns input from all players (both local and remote) from the input queue
// Which can be a prediction (i.e the last frame) or the latest input recieved
// Also used to fetch inputs from GGPO  to update the game states during the advance
// frame callback
func (p *Peer2PeerBackend) SyncInput(disconnectFlags *int) ([][]byte, GGTHXErrorCode) {
	// Wait until we've started to return inputs.
	if p.synchronizing {
		return nil, GGTHX_ERRORCODE_NOT_SYNCHRONIZED
	}
	values, flags := p.sync.SynchronizeInputs()
	if disconnectFlags != nil {
		*disconnectFlags = flags
	}
	return values, GGTHX_OK
}

// Maps to top level API AdvanceFrame function
// sync.IncremenetFrame increments the frame count and saves the
// current state via user provided callback
// Do Poll Not only runs everything in the system that's registered to poll
// it... well does everything. I'll get ti it when I get to it.
func (p *Peer2PeerBackend) IncrementFrame() GGTHXErrorCode {
	log.Printf("End of frame (%d)...\n", p.sync.GetFrameCount())
	p.sync.IncrementFrame()
	p.DoPoll(0)
	p.PollSyncEvents()

	return GGTHX_OK
}

// We don't do anything with these events in the P2PBackend for sure,
// but I can't seem to find anywhere in GGPO that actually sync events
func (p *Peer2PeerBackend) PollSyncEvents() {
	var e SyncEvent
	for p.sync.GetEvent(&e) {
		p.OnSyncEvent(&e)
	}
	return
}

// Handles all the events  for all spactors and players. Done OnPoll
func (p *Peer2PeerBackend) PollUdpProtocolEvents() {
	for i := 0; i < p.numPlayers; i++ {
		for {
			evt, err := p.endpoints[i].GetEvent()
			if err != nil {
				break
			} else {
				p.OnUdpProtocolPeerEvent(evt, i)
			}
		}
	}
	for i := 0; i < p.numSpectators; i++ {
		for {
			evt, err := p.endpoints[i].GetEvent()
			if err != nil {
				break
			} else {
				p.OnUdpProtocolSpectatorEvent(evt, i)
			}
		}
	}
}

// Takes events that come from UdpProtocol and if they are input,
// Sends remote input to Sync which adds it to its InputQueue
// Also updates that lastFrame for this endpoint as the most
// recently recived frame.
// Disconnects if necesary
func (p *Peer2PeerBackend) OnUdpProtocolPeerEvent(evt *UdpProtocolEvent, queue int) {
	handle := p.QueueToPlayerHandle(queue)
	p.OnUdpProtocolEvent(evt, handle)
	switch evt.eventType {
	case InputEvent:
		if p.localConnectStatus[queue].Disconnected != 0 {
			currentRemoteFrame := p.localConnectStatus[queue].LastFrame
			newRemoteFrame := evt.input.Frame
			Assert(currentRemoteFrame == -1 || newRemoteFrame == (currentRemoteFrame+1))

			p.sync.AddRemoteInput(queue, &evt.input)
			// Notify the other endpoints which frame we received from a peer
			log.Printf("setting remote connect status for queue %d to %d\n", queue,
				evt.input.Frame)
			p.localConnectStatus[queue].LastFrame = evt.input.Frame
		}
	case DisconnectedEvent:
		p.DisconnectPlayer(handle)
	}
}

// Every DoPoll, every endpoint and spectator goes through its event queue
// handles each event and pops it from the queue.  Though most of the logic
// for handling these events is the same (see: OnUdpProtocolEvent ), spectators
// and peers handle certain events differently
func (p *Peer2PeerBackend) OnUdpProtocolSpectatorEvent(evt *UdpProtocolEvent, queue int) {
	handle := p.QueueToSpectatorHandle(queue)
	p.OnUdpProtocolEvent(evt, handle)

	var info GGTHXEvent
	switch evt.eventType {
	case DisconnectedEvent:
		p.spectators[queue].Disconnect()

		info.Code = GGTHX_EVENTCODE_DISCONNECTED_FROM_PEER
		info.player = handle
		p.callbacks.OnEvent(&info)
	}
}

// Logic for parsing UdpProtocol events and sending them up to the user via callbacks.
// In P2P Backend, called by OnUdpProtocolSpectatorEvent and OnUdpProtocolPeerEvent,
// which themselves are called by PollUdpProtocolEvents, which happens every DoPoll
func (p *Peer2PeerBackend) OnUdpProtocolEvent(evt *UdpProtocolEvent, handle GGTHXPlayerHandle) {
	var info GGTHXEvent

	switch evt.eventType {
	case ConnectedEvent:
		info.Code = GGTHX_EVENTCODE_CONNECTED_TO_PEER
		info.player = handle
		p.callbacks.OnEvent(&info)

	case SynchronizingEvent:
		info.Code = GGTHX_EVENTCODE_SYNCHRONIZING_WITH_PEER
		info.player = handle
		info.count = evt.count
		info.total = evt.total
		p.callbacks.OnEvent(&info)

	case SynchronziedEvent:
		info.Code = GGTHX_EVENTCODE_SYNCHRONIZED_WITH_PEER
		info.player = handle
		p.callbacks.OnEvent(&info)

		p.CheckInitialSync()

	case NetworkInterruptedEvent:
		info.Code = GGTHX_EVENTCODE_CONNECTION_INTERRUPTED
		info.player = handle
		info.disconnectTimeout = evt.disconnectTimeout
		p.callbacks.OnEvent(&info)

	case NetworkResumedEvent:
		info.Code = GGTHX_EVENTCODE_CONNECTION_RESUMED
		info.player = handle
		p.callbacks.OnEvent(&info)
	}
}

/*
 * Called only as the result of a local decision to disconnect.  The remote
 * decisions to disconnect are a result of us parsing the peer_connect_settings
 * blob in every endpoint periodically.
 * - pond3r
	This is the function that's called when the UdpProtocol endpoint recogniizes
	a disconnect (that lastRecvTime + disconnectTimeout < now) and sends that event
	up to the backend.
    Also maps to API function
*/
func (p *Peer2PeerBackend) DisconnectPlayer(player GGTHXPlayerHandle) GGTHXErrorCode {
	var queue int

	result := p.PlayerHandleToQueue(player, &queue)
	if !GGTHX_SUCESS(result) {
		return result
	}

	if p.localConnectStatus[queue].Disconnected > 0 {
		return GGTHX_ERRORCODE_PLAYER_DISCONNECTED
	}

	if !p.endpoints[queue].IsInitialized() {
		currentFrame := p.sync.GetFrameCount()
		// xxx: we should be tracking who the local player is, but for now assume
		// that if the endpoint is not initalized, this must be the local player. - pond3r

		// 100% sure this assumption won't be applicable for me, but we'll see.
		// Not that it matters lol
		log.Printf("Disconnecting local player %d at frame %d by user request.\n",
			queue, p.localConnectStatus[queue].LastFrame)
		// Disconnecting all the other players too
		for i := 0; i < p.numPlayers; i++ {
			if p.endpoints[i].IsInitialized() {
				p.DisconnectPlayerQueue(i, currentFrame)
			}
		}
	} else {
		log.Printf("Disconnecting queue %d at frame %d by user request.\n",
			queue, p.localConnectStatus[queue].LastFrame)
		p.DisconnectPlayerQueue(queue, p.localConnectStatus[queue].LastFrame)
	}
	return GGTHX_OK
}

/*
	Sets the enpoints' state to disconnected.
	Also sets localConnectStatus to disconnected which is used all over the p2p backend to
	verify if an endpoint is connected
	And adjusts the simulation to get to syncto frames
	Then sends the Disconnect Event up to te user.
*/
func (p *Peer2PeerBackend) DisconnectPlayerQueue(queue int, syncto int) {
	var info GGTHXEvent
	frameCount := p.sync.GetFrameCount()

	p.endpoints[queue].Disconnect()

	log.Printf("Changing queue %d local connect status for last frame from %d to %d on disconnect request (current: %d).\n",
		queue, p.localConnectStatus[queue].LastFrame, syncto, frameCount)

	p.localConnectStatus[queue].Disconnected = 1
	p.localConnectStatus[queue].LastFrame = syncto

	if syncto < frameCount {
		log.Printf("adjusting simulation to account for the fact that %d disconnected @ %d.\n", queue, syncto)
		p.sync.AdjustSimulation(syncto)
		log.Printf("Finished adjusting simulation.\n")
	}

	info.Code = GGTHX_EVENTCODE_DISCONNECTED_FROM_PEER
	info.player = p.QueueToPlayerHandle(queue)
	p.callbacks.OnEvent(&info)

	p.CheckInitialSync()
}

/*
	Gets network stats for that specific play from their UdpProtocol Endpoint
	Includes ping, sendQueLen, kbpsSent, remoteFramesBehind and remoteFrameAdvantage
	All coming from the UdpProtocol Endpoint
	Maps to top level API function.
*/
func (p *Peer2PeerBackend) GetNetworkStats(stats *GGTHXNetworkStats, player GGTHXPlayerHandle) GGTHXErrorCode {
	var queue int

	result := p.PlayerHandleToQueue(player, &queue)
	if !GGTHX_SUCESS(result) {
		return result
	}

	p.endpoints[queue].GetNetworkStats(stats)

	return GGTHX_OK
}

/*
	Sets frame delay for that specific player's input queue in Sync.
	Frame delay is used in the input queue, when remote inputs are recieved from
	the UdpProtocol and sent to Sync, sync then adds those inputs to the input queue
	for that specific player, and sort of artificially corrects the frame that player
	should be on by increasing it frameDelay amount
	Maps to top level API function
*/
func (p *Peer2PeerBackend) SetFrameDelay(player GGTHXPlayerHandle, delay int) GGTHXErrorCode {
	var queue int

	result := p.PlayerHandleToQueue(player, &queue)
	if !GGTHX_SUCESS(result) {
		return result
	}
	p.sync.SetFrameDelay(queue, delay)
	return GGTHX_OK
}

/*
	Propagates the disconnect timeout to all of the endpoints.
    lastRecvTime + disconnectTimeout < now means the endpoint has stopped
	recieving packets and we are now disconnecting, effectively timing out.
	The Udp endpoint propogates the Disconnect Event up to the backend.
	Which, in the P2P Backend, Disconnects the Player from the backend,
	then sends the event upward.
	Mapped to top level API function
*/
func (p *Peer2PeerBackend) SetDisconnectTimeout(timeout int) GGTHXErrorCode {
	p.disconnectTimeout = timeout
	for i := 0; i < p.numPlayers; i++ {
		if p.endpoints[i].IsInitialized() {
			p.endpoints[i].SetDisconnectTimeout(p.disconnectTimeout)
		}
	}
	return GGTHX_OK
}

/*
	Propagates the disconnect notify start to all of the endpoints
	lastRecTime + disconnectNotifyStart < now  means the endpoint has
	stopped recieving packets. The udp endpoint starts sending a NetworkInterrupted event
	up to the backend, check sends it up to the user via the API's callbacks.
	Mapped to top level Api function
*/
func (p *Peer2PeerBackend) SetDisconnectNotifyStart(timeout int) GGTHXErrorCode {
	p.disconnectNotifyStart = timeout
	for i := 0; i < p.numPlayers; i++ {
		if p.endpoints[i].IsInitialized() {
			p.endpoints[i].SetDisconnectNotifyStart(p.disconnectNotifyStart)
		}
	}
	return GGTHX_OK
}

/*
	Used for getting the index for PlayerHandle mapped arrays such as
	endpoints, localConnectStatus, andinputQueues in Sync
*/
func (p *Peer2PeerBackend) PlayerHandleToQueue(player GGTHXPlayerHandle, queue *int) GGTHXErrorCode {
	offset := int(player) - 1
	if offset < 0 || offset >= p.numPlayers {
		return GGTHX_ERRORCODE_INVALID_PLAYER_HANDLE
	}
	*queue = offset
	return GGTHX_OK
}

/*
	Does the inverse of the above. Turns index index into human readible number
*/
func (p *Peer2PeerBackend) QueueToPlayerHandle(queue int) GGTHXPlayerHandle {
	return GGTHXPlayerHandle(queue + 1)
}

func (p *Peer2PeerBackend) QueueToSpectatorHandle(queue int) GGTHXPlayerHandle {
	return GGTHXPlayerHandle(queue + 1000) /* out of range of the player array, basically  - pond3r*/
}

/*
	Propogates messages to all endpoints and spectators (?)
	As of right now it hands the message off to the first endpoint that
	handles it then returns?
*/
func (p *Peer2PeerBackend) OnMsg(msg *UdpMsg, length int) {
	for i := 0; i < p.numPlayers; i++ {
		if p.endpoints[i].HandlesMsg() {
			p.endpoints[i].OnMsg(msg, length)
			return
		}
	}
	for i := 0; i < p.numSpectators; i++ {
		if p.spectators[i].HandlesMsg() {
			p.spectators[i].OnMsg(msg, length)
			return
		}
	}
}

/*
	Checks if all endpoints and spectators are initialized and synchronized
	and sends an event when they are.
*/
func (p *Peer2PeerBackend) CheckInitialSync() {
	var i int

	if p.synchronizing {
		// Check to see if everyone is now synchronized. If so,
		// go and tell the client that we're ok to accept in.
		for i = 0; i < p.numPlayers; i++ {
			// xxx: IsInitialized() must go... we're actually using it as a proxy for "represents the local player" -pond3r
			if p.endpoints[i].IsInitialized() && !p.endpoints[i].IsSynchronized() && p.localConnectStatus[i].Disconnected == 0 {
				return
			}
		}

		for i = 0; i < p.numSpectators; i++ {
			if p.spectators[i].IsInitialized() && !p.spectators[i].IsSynchronized() {
				return
			}
		}

		var info GGTHXEvent
		info.Code = GGTHX_EVENTCODE_RUNNING
		p.callbacks.OnEvent(&info)
		p.synchronizing = false
	}
}

func (p *Peer2PeerBackend) OnSyncEvent(e *SyncEvent) {
	// stub function as it was in GGPO
}

func (p *Peer2PeerBackend) Chat(text string) GGTHXErrorCode {
	return GGTHX_OK
}

func (p *Peer2PeerBackend) Logv(format string, args ...int) GGTHXErrorCode {
	return GGTHX_OK
}
