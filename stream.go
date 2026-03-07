package kvm

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v4/pkg/media"
)

var (
	videoStreamSubscribersMu sync.RWMutex
	videoStreamSubscribers   = map[chan []byte]struct{}{}

	streamViewerSessionsMu sync.RWMutex
	streamViewerSessions   = map[*Session]struct{}{}
)

func getActiveStreamClients() int {
	videoStreamSubscribersMu.RLock()
	defer videoStreamSubscribersMu.RUnlock()

	return len(videoStreamSubscribers)
}

func hasActiveVideoConsumers() bool {
	return getActiveSessions() > 0 || getActiveStreamClients() > 0
}

func onFirstVideoConsumerConnected() {
	_ = nativeInstance.VideoStart()
	stopVideoSleepModeTicker()
}

func onLastVideoConsumerDisconnected() {
	_ = nativeInstance.VideoStop()
	startVideoSleepModeTicker()
}

func subscribeVideoStream() chan []byte {
	ch := make(chan []byte, 4)

	videoStreamSubscribersMu.Lock()
	videoStreamSubscribers[ch] = struct{}{}
	activeStreamClients := len(videoStreamSubscribers)
	videoStreamSubscribersMu.Unlock()

	if activeStreamClients == 1 && getActiveSessions() == 0 {
		onFirstVideoConsumerConnected()
	}

	return ch
}

func unsubscribeVideoStream(ch chan []byte) {
	videoStreamSubscribersMu.Lock()
	delete(videoStreamSubscribers, ch)
	activeStreamClients := len(videoStreamSubscribers)
	videoStreamSubscribersMu.Unlock()

	if activeStreamClients == 0 && getActiveSessions() == 0 {
		onLastVideoConsumerDisconnected()
	}
}

func publishVideoFrame(frame []byte) {
	videoStreamSubscribersMu.RLock()
	if len(videoStreamSubscribers) == 0 {
		videoStreamSubscribersMu.RUnlock()
		return
	}

	frameCopy := append([]byte(nil), frame...)
	for subscriber := range videoStreamSubscribers {
		select {
		case subscriber <- frameCopy:
		default:
			// Drop frames for slow clients to keep live latency low.
		}
	}
	videoStreamSubscribersMu.RUnlock()
}

func registerStreamViewerSession(session *Session) {
	streamViewerSessionsMu.Lock()
	streamViewerSessions[session] = struct{}{}
	streamViewerSessionsMu.Unlock()
}

func getActiveStreamViewerSessions() int {
	streamViewerSessionsMu.RLock()
	defer streamViewerSessionsMu.RUnlock()

	return len(streamViewerSessions)
}

func unregisterStreamViewerSession(session *Session) {
	streamViewerSessionsMu.Lock()
	delete(streamViewerSessions, session)
	streamViewerSessionsMu.Unlock()
}

func publishVideoToStreamViewerSessions(frame []byte, duration time.Duration) {
	streamViewerSessionsMu.RLock()
	if len(streamViewerSessions) == 0 {
		streamViewerSessionsMu.RUnlock()
		return
	}

	sessions := make([]*Session, 0, len(streamViewerSessions))
	for session := range streamViewerSessions {
		sessions = append(sessions, session)
	}
	streamViewerSessionsMu.RUnlock()

	for _, session := range sessions {
		if session == nil || session.VideoTrack == nil {
			continue
		}

		if err := session.VideoTrack.WriteSample(media.Sample{Data: frame, Duration: duration}); err != nil {
			nativeLogger.Warn().Err(err).Msg("error writing sample to stream viewer session")
		}
	}
}

const streamViewerHTML = `<!doctype html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>Video Stream</title>
	<style>
		:root {
			--bg: #0f1419;
			--panel: #1a222c;
			--text: #f3f5f7;
			--muted: #9aa6b2;
			--accent: #2bb1ff;
			--danger: #ff6b6b;
		}
		html, body {
			margin: 0;
			padding: 0;
			width: 100%;
			height: 100%;
			background: radial-gradient(circle at 20% 10%, #233140 0%, var(--bg) 42%);
			color: var(--text);
			font-family: "IBM Plex Sans", "Segoe UI", sans-serif;
		}
		.layout {
			display: flex;
			flex-direction: column;
			gap: 12px;
			height: 100%;
			padding: 12px;
			box-sizing: border-box;
		}
		.toolbar {
			display: flex;
			gap: 10px;
			align-items: center;
			background: color-mix(in srgb, var(--panel) 82%, transparent);
			border: 1px solid #2f3b48;
			border-radius: 12px;
			padding: 10px;
		}
		.title {
			font-size: 14px;
			letter-spacing: 0.04em;
			text-transform: uppercase;
			color: var(--muted);
			margin-right: auto;
		}
		button {
			border: 1px solid #3c4b5b;
			background: #243140;
			color: var(--text);
			padding: 8px 12px;
			border-radius: 8px;
			cursor: pointer;
			font-weight: 600;
		}
		button:hover {
			border-color: var(--accent);
		}
		#status {
			font-size: 13px;
			color: var(--muted);
		}
		.stage {
			flex: 1;
			min-height: 0;
			border-radius: 12px;
			overflow: hidden;
			border: 1px solid #2f3b48;
			background: #000;
			position: relative;
		}
		video {
			width: 100%;
			height: 100%;
			object-fit: contain;
			background: #000;
		}
		.error {
			color: var(--danger);
		}
	</style>
</head>
<body>
	<main class="layout">
		<section class="toolbar">
			<div class="title">Video Stream</div>
			<div id="status">Initializing...</div>
			<button id="reconnect" type="button">Reconnect</button>
			<button id="fullscreen" type="button">Fullscreen</button>
		</section>
		<section class="stage" id="stage">
			<video id="video" autoplay playsinline muted></video>
		</section>
	</main>
	<script>
		const statusEl = document.getElementById("status");
		const videoEl = document.getElementById("video");
		const stageEl = document.getElementById("stage");
		const reconnectEl = document.getElementById("reconnect");
		const fullscreenEl = document.getElementById("fullscreen");

		let pc = null;
		let reconnectTimer = null;
		let reconnectAttempts = 0;
		let intentionalClose = false;

		const reconnectConfig = {
			initialDelayMs: 1000,
			maxDelayMs: 10000,
			fetchTimeoutMs: 10000,
		};

		function setStatus(message, isError = false) {
			statusEl.textContent = message;
			statusEl.className = isError ? "error" : "";
		}

		function waitForIceGatheringComplete(peerConnection) {
			if (peerConnection.iceGatheringState === "complete") {
				return Promise.resolve();
			}
			return new Promise((resolve) => {
				const onStateChange = () => {
					if (peerConnection.iceGatheringState === "complete") {
						peerConnection.removeEventListener("icegatheringstatechange", onStateChange);
						resolve();
					}
				};
				peerConnection.addEventListener("icegatheringstatechange", onStateChange);
			});
		}

		function clearReconnectTimer() {
			if (reconnectTimer) {
				clearTimeout(reconnectTimer);
				reconnectTimer = null;
			}
		}

		function closePeerConnection() {
			if (pc) {
				try {
					pc.ontrack = null;
					pc.oniceconnectionstatechange = null;
					pc.onconnectionstatechange = null;
					pc.close();
				} catch (_) {}
				pc = null;
			}
		}

		function scheduleReconnect(reason) {
			if (intentionalClose) {
				return;
			}
			if (reconnectTimer) {
				return;
			}

			const delay = Math.min(
				reconnectConfig.initialDelayMs * Math.pow(2, reconnectAttempts),
				reconnectConfig.maxDelayMs,
			);

			reconnectAttempts += 1;
			setStatus("Reconnecting in " + Math.ceil(delay / 1000) + "s (" + reason + ")", true);

			reconnectTimer = setTimeout(() => {
				reconnectTimer = null;
				connect().catch((error) => {
					scheduleReconnect(error.message || "connect error");
				});
			}, delay);
		}

		async function connect() {
			clearReconnectTimer();
			closePeerConnection();

			pc = new RTCPeerConnection();
			pc.addTransceiver("video", { direction: "recvonly" });

			pc.ontrack = (event) => {
				console.log("ontrack event:", event);
				setStatus("Track received");
				reconnectAttempts = 0;
				if (event.streams && event.streams[0]) {
					videoEl.srcObject = event.streams[0];
					console.log("Video source set");
				}
			};

			pc.oniceconnectionstatechange = () => {
				console.log("ICE state:", pc.iceConnectionState);
				setStatus("ICE: " + pc.iceConnectionState);

				if (["failed", "disconnected", "closed"].includes(pc.iceConnectionState)) {
					scheduleReconnect("ice " + pc.iceConnectionState);
				}
			};

			pc.onconnectionstatechange = () => {
				console.log("Connection state:", pc.connectionState);
				setStatus("Connection: " + pc.connectionState);

				if (["failed", "disconnected", "closed"].includes(pc.connectionState)) {
					scheduleReconnect("peer " + pc.connectionState);
				}
			};

			setStatus("Creating offer...");
			const offer = await pc.createOffer();
			await pc.setLocalDescription(offer);
			await waitForIceGatheringComplete(pc);

			const encodedOffer = btoa(JSON.stringify(pc.localDescription));
			setStatus("Requesting session...");
			const controller = new AbortController();
			const timeout = setTimeout(() => controller.abort(), reconnectConfig.fetchTimeoutMs);
			const response = await fetch("/webrtc/stream-session", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				credentials: "same-origin",
				body: JSON.stringify({ sd: encodedOffer }),
				signal: controller.signal,
			});
			clearTimeout(timeout);

			if (!response.ok) {
				const errorText = await response.text();
				throw new Error("Session error " + response.status + ": " + errorText);
			}

			setStatus("Processing answer...");
			const payload = await response.json();
			const answerJson = atob(payload.sd);
			const answer = JSON.parse(answerJson);
			await pc.setRemoteDescription(answer);

			setStatus("Waiting for connection...");
			reconnectAttempts = 0;
		}

		reconnectEl.addEventListener("click", async () => {
			try {
				reconnectAttempts = 0;
				setStatus("Reconnecting...");
				await connect();
			} catch (error) {
				scheduleReconnect(error.message || "Reconnect failed");
			}
		});

		fullscreenEl.addEventListener("click", async () => {
			try {
				if (document.fullscreenElement) {
					await document.exitFullscreen();
				} else {
					if (stageEl.requestFullscreen) {
						await stageEl.requestFullscreen();
					} else if (stageEl.webkitRequestFullscreen) {
						stageEl.webkitRequestFullscreen();
					}
				}
			} catch (error) {
				setStatus(error.message || "Fullscreen failed", true);
			}
		});

		window.addEventListener("offline", () => {
			setStatus("Network offline", true);
		});

		window.addEventListener("online", () => {
			scheduleReconnect("network online");
		});

		window.addEventListener("beforeunload", () => {
			intentionalClose = true;
			clearReconnectTimer();
			closePeerConnection();
		});

		connect().catch((error) => {
			scheduleReconnect(error.message || "Connection failed");
		});
	</script>
</body>
</html>`

const streamViewerTestHTML = `<!doctype html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>Video Stream Test</title>
	<style>
		:root {
			--bg: #0f1419;
			--panel: #1a222c;
			--text: #f3f5f7;
			--muted: #9aa6b2;
			--accent: #2bb1ff;
			--danger: #ff6b6b;
		}
		html, body {
			margin: 0;
			padding: 0;
			width: 100%;
			height: 100%;
			background: radial-gradient(circle at 20% 10%, #233140 0%, var(--bg) 42%);
			color: var(--text);
			font-family: "IBM Plex Sans", "Segoe UI", sans-serif;
		}
		.layout {
			display: grid;
			grid-template-rows: auto minmax(0, 1fr);
			gap: 12px;
			height: 100%;
			padding: 12px;
			box-sizing: border-box;
		}
		.toolbar {
			display: flex;
			gap: 10px;
			align-items: center;
			background: color-mix(in srgb, var(--panel) 82%, transparent);
			border: 1px solid #2f3b48;
			border-radius: 12px;
			padding: 10px;
		}
		.title {
			font-size: 14px;
			letter-spacing: 0.04em;
			text-transform: uppercase;
			color: var(--muted);
			margin-right: auto;
		}
		button {
			border: 1px solid #3c4b5b;
			background: #243140;
			color: var(--text);
			padding: 8px 12px;
			border-radius: 8px;
			cursor: pointer;
			font-weight: 600;
		}
		button:hover {
			border-color: var(--accent);
		}
		#status {
			font-size: 13px;
			color: var(--muted);
		}
		.content {
			display: grid;
			grid-template-columns: minmax(0, 1fr) 360px;
			gap: 12px;
			min-height: 0;
		}
		.stage {
			min-height: 0;
			border-radius: 12px;
			overflow: hidden;
			border: 1px solid #2f3b48;
			background: #000;
			position: relative;
		}
		video {
			width: 100%;
			height: 100%;
			object-fit: contain;
			background: #000;
		}
		.panel {
			border-radius: 12px;
			border: 1px solid #2f3b48;
			background: color-mix(in srgb, var(--panel) 90%, transparent);
			padding: 12px;
			overflow: auto;
		}
		.panel h2 {
			margin: 0 0 8px 0;
			font-size: 13px;
			letter-spacing: 0.04em;
			text-transform: uppercase;
			color: var(--muted);
		}
		.stats-grid {
			display: grid;
			grid-template-columns: 1fr;
			gap: 8px;
		}
		.stat {
			display: flex;
			justify-content: space-between;
			gap: 12px;
			font-size: 13px;
			padding: 6px 8px;
			border-radius: 8px;
			background: #111922;
		}
		.stat .label {
			color: var(--muted);
		}
		.stat .value {
			font-family: "IBM Plex Mono", "SFMono-Regular", Consolas, monospace;
			text-align: right;
		}
		.error {
			color: var(--danger);
		}
		@media (max-width: 980px) {
			.content {
				grid-template-columns: 1fr;
				grid-template-rows: minmax(0, 1fr) auto;
			}
			.panel {
				max-height: 42vh;
			}
		}
	</style>
</head>
<body>
	<main class="layout">
		<section class="toolbar">
			<div class="title">Video Stream Test</div>
			<div id="status">Initializing...</div>
			<button id="reconnect" type="button">Reconnect</button>
			<button id="fullscreen" type="button">Fullscreen</button>
		</section>
		<section class="content">
			<section class="stage" id="stage">
				<video id="video" autoplay playsinline muted></video>
			</section>
			<aside class="panel">
				<h2>Diagnostics</h2>
				<div class="stats-grid">
					<div class="stat"><span class="label">Connection State</span><span class="value" id="stat-connection-state">-</span></div>
					<div class="stat"><span class="label">ICE State</span><span class="value" id="stat-ice-state">-</span></div>
					<div class="stat"><span class="label">Codec</span><span class="value" id="stat-codec">-</span></div>
					<div class="stat"><span class="label">Resolution</span><span class="value" id="stat-resolution">-</span></div>
					<div class="stat"><span class="label">Frame Rate</span><span class="value" id="stat-fps">-</span></div>
					<div class="stat"><span class="label">Bitrate</span><span class="value" id="stat-bitrate">-</span></div>
					<div class="stat"><span class="label">Packets Received</span><span class="value" id="stat-packets-received">-</span></div>
					<div class="stat"><span class="label">Packets Lost</span><span class="value" id="stat-packets-lost">-</span></div>
					<div class="stat"><span class="label">Jitter</span><span class="value" id="stat-jitter">-</span></div>
					<div class="stat"><span class="label">RTT</span><span class="value" id="stat-rtt">-</span></div>
					<div class="stat"><span class="label">NACK Count</span><span class="value" id="stat-nack">-</span></div>
					<div class="stat"><span class="label">PLI Count</span><span class="value" id="stat-pli">-</span></div>
				</div>
			</aside>
		</section>
	</main>
	<script>
		const statusEl = document.getElementById("status");
		const videoEl = document.getElementById("video");
		const stageEl = document.getElementById("stage");
		const reconnectEl = document.getElementById("reconnect");
		const fullscreenEl = document.getElementById("fullscreen");

		const statEls = {
			connectionState: document.getElementById("stat-connection-state"),
			iceState: document.getElementById("stat-ice-state"),
			codec: document.getElementById("stat-codec"),
			resolution: document.getElementById("stat-resolution"),
			fps: document.getElementById("stat-fps"),
			bitrate: document.getElementById("stat-bitrate"),
			packetsReceived: document.getElementById("stat-packets-received"),
			packetsLost: document.getElementById("stat-packets-lost"),
			jitter: document.getElementById("stat-jitter"),
			rtt: document.getElementById("stat-rtt"),
			nack: document.getElementById("stat-nack"),
			pli: document.getElementById("stat-pli"),
		};

		let pc = null;
		let reconnectTimer = null;
		let reconnectAttempts = 0;
		let intentionalClose = false;
		let statsTimer = null;
		let prevInboundBytes = null;
		let prevInboundTimestamp = null;

		const reconnectConfig = {
			initialDelayMs: 1000,
			maxDelayMs: 10000,
			fetchTimeoutMs: 10000,
		};

		function setStatus(message, isError = false) {
			statusEl.textContent = message;
			statusEl.className = isError ? "error" : "";
		}

		function setStat(key, value) {
			if (!statEls[key]) {
				return;
			}
			statEls[key].textContent = value;
		}

		function formatBitrate(bps) {
			if (bps == null || Number.isNaN(bps)) {
				return "-";
			}
			if (bps >= 1_000_000) {
				return (bps / 1_000_000).toFixed(2) + " Mbps";
			}
			if (bps >= 1_000) {
				return (bps / 1_000).toFixed(1) + " Kbps";
			}
			return Math.round(bps) + " bps";
		}

		function waitForIceGatheringComplete(peerConnection) {
			if (peerConnection.iceGatheringState === "complete") {
				return Promise.resolve();
			}
			return new Promise((resolve) => {
				const onStateChange = () => {
					if (peerConnection.iceGatheringState === "complete") {
						peerConnection.removeEventListener("icegatheringstatechange", onStateChange);
						resolve();
					}
				};
				peerConnection.addEventListener("icegatheringstatechange", onStateChange);
			});
		}

		function clearReconnectTimer() {
			if (reconnectTimer) {
				clearTimeout(reconnectTimer);
				reconnectTimer = null;
			}
		}

		function clearStatsTimer() {
			if (statsTimer) {
				clearInterval(statsTimer);
				statsTimer = null;
			}
		}

		function resetStatDisplay() {
			setStat("codec", "-");
			setStat("resolution", "-");
			setStat("fps", "-");
			setStat("bitrate", "-");
			setStat("packetsReceived", "-");
			setStat("packetsLost", "-");
			setStat("jitter", "-");
			setStat("rtt", "-");
			setStat("nack", "-");
			setStat("pli", "-");
		}

		function closePeerConnection() {
			clearStatsTimer();
			resetStatDisplay();
			prevInboundBytes = null;
			prevInboundTimestamp = null;
			if (pc) {
				try {
					pc.ontrack = null;
					pc.oniceconnectionstatechange = null;
					pc.onconnectionstatechange = null;
					pc.close();
				} catch (_) {}
				pc = null;
			}
		}

		function scheduleReconnect(reason) {
			if (intentionalClose) {
				return;
			}
			if (reconnectTimer) {
				return;
			}

			const delay = Math.min(
				reconnectConfig.initialDelayMs * Math.pow(2, reconnectAttempts),
				reconnectConfig.maxDelayMs,
			);

			reconnectAttempts += 1;
			setStatus("Reconnecting in " + Math.ceil(delay / 1000) + "s (" + reason + ")", true);

			reconnectTimer = setTimeout(() => {
				reconnectTimer = null;
				connect().catch((error) => {
					scheduleReconnect(error.message || "connect error");
				});
			}, delay);
		}

		function startStatsLoop() {
			clearStatsTimer();
			statsTimer = setInterval(async () => {
				if (!pc) {
					return;
				}

				try {
					const reports = await pc.getStats();
					let inboundVideo = null;
					let selectedCandidatePair = null;
					let codec = null;

					reports.forEach((report) => {
						if (report.type === "inbound-rtp" && report.kind === "video") {
							inboundVideo = report;
						}
						if (report.type === "candidate-pair" && report.nominated && report.state === "succeeded") {
							selectedCandidatePair = report;
						}
					});

					if (inboundVideo && inboundVideo.codecId && reports.get(inboundVideo.codecId)) {
						codec = reports.get(inboundVideo.codecId);
					}

					if (codec && codec.mimeType) {
						setStat("codec", codec.mimeType);
					}

					if (inboundVideo) {
						if (typeof inboundVideo.frameWidth === "number" && typeof inboundVideo.frameHeight === "number") {
							setStat("resolution", inboundVideo.frameWidth + "x" + inboundVideo.frameHeight);
						}

						if (typeof inboundVideo.framesPerSecond === "number") {
							setStat("fps", inboundVideo.framesPerSecond.toFixed(1) + " fps");
						}

						if (typeof inboundVideo.packetsReceived === "number") {
							setStat("packetsReceived", String(inboundVideo.packetsReceived));
						}

						if (typeof inboundVideo.packetsLost === "number") {
							setStat("packetsLost", String(inboundVideo.packetsLost));
						}

						if (typeof inboundVideo.jitter === "number") {
							setStat("jitter", (inboundVideo.jitter * 1000).toFixed(2) + " ms");
						}

						if (typeof inboundVideo.nackCount === "number") {
							setStat("nack", String(inboundVideo.nackCount));
						}

						if (typeof inboundVideo.pliCount === "number") {
							setStat("pli", String(inboundVideo.pliCount));
						}

						if (typeof inboundVideo.bytesReceived === "number" && typeof inboundVideo.timestamp === "number") {
							if (prevInboundBytes != null && prevInboundTimestamp != null) {
								const bits = (inboundVideo.bytesReceived - prevInboundBytes) * 8;
								const seconds = (inboundVideo.timestamp - prevInboundTimestamp) / 1000;
								if (seconds > 0 && bits >= 0) {
									setStat("bitrate", formatBitrate(bits / seconds));
								}
							}
							prevInboundBytes = inboundVideo.bytesReceived;
							prevInboundTimestamp = inboundVideo.timestamp;
						}
					}

					if (selectedCandidatePair && typeof selectedCandidatePair.currentRoundTripTime === "number") {
						setStat("rtt", (selectedCandidatePair.currentRoundTripTime * 1000).toFixed(1) + " ms");
					}
				} catch (_) {
					// Keep streaming even when transient getStats errors happen.
				}
			}, 1000);
		}

		async function connect() {
			clearReconnectTimer();
			closePeerConnection();

			pc = new RTCPeerConnection();
			pc.addTransceiver("video", { direction: "recvonly" });
			setStat("connectionState", pc.connectionState || "new");
			setStat("iceState", pc.iceConnectionState || "new");

			pc.ontrack = (event) => {
				setStatus("Track received");
				reconnectAttempts = 0;
				if (event.streams && event.streams[0]) {
					videoEl.srcObject = event.streams[0];
				}
				startStatsLoop();
			};

			pc.oniceconnectionstatechange = () => {
				setStatus("ICE: " + pc.iceConnectionState);
				setStat("iceState", pc.iceConnectionState);

				if (["failed", "disconnected", "closed"].includes(pc.iceConnectionState)) {
					scheduleReconnect("ice " + pc.iceConnectionState);
				}
			};

			pc.onconnectionstatechange = () => {
				setStatus("Connection: " + pc.connectionState);
				setStat("connectionState", pc.connectionState);

				if (["failed", "disconnected", "closed"].includes(pc.connectionState)) {
					scheduleReconnect("peer " + pc.connectionState);
				}
			};

			setStatus("Creating offer...");
			const offer = await pc.createOffer();
			await pc.setLocalDescription(offer);
			await waitForIceGatheringComplete(pc);

			const encodedOffer = btoa(JSON.stringify(pc.localDescription));
			setStatus("Requesting session...");
			const controller = new AbortController();
			const timeout = setTimeout(() => controller.abort(), reconnectConfig.fetchTimeoutMs);
			const response = await fetch("/webrtc/stream-session", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				credentials: "same-origin",
				body: JSON.stringify({ sd: encodedOffer }),
				signal: controller.signal,
			});
			clearTimeout(timeout);

			if (!response.ok) {
				const errorText = await response.text();
				throw new Error("Session error " + response.status + ": " + errorText);
			}

			setStatus("Processing answer...");
			const payload = await response.json();
			const answerJson = atob(payload.sd);
			const answer = JSON.parse(answerJson);
			await pc.setRemoteDescription(answer);

			setStatus("Waiting for connection...");
			reconnectAttempts = 0;
		}

		reconnectEl.addEventListener("click", async () => {
			try {
				reconnectAttempts = 0;
				setStatus("Reconnecting...");
				await connect();
			} catch (error) {
				scheduleReconnect(error.message || "Reconnect failed");
			}
		});

		fullscreenEl.addEventListener("click", async () => {
			try {
				if (document.fullscreenElement) {
					await document.exitFullscreen();
				} else {
					if (stageEl.requestFullscreen) {
						await stageEl.requestFullscreen();
					} else if (stageEl.webkitRequestFullscreen) {
						stageEl.webkitRequestFullscreen();
					}
				}
			} catch (error) {
				setStatus(error.message || "Fullscreen failed", true);
			}
		});

		window.addEventListener("offline", () => {
			setStatus("Network offline", true);
		});

		window.addEventListener("online", () => {
			scheduleReconnect("network online");
		});

		window.addEventListener("beforeunload", () => {
			intentionalClose = true;
			clearReconnectTimer();
			closePeerConnection();
		});

		connect().catch((error) => {
			scheduleReconnect(error.message || "Connection failed");
		});
	</script>
</body>
</html>`

func handleStreamPage(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(streamViewerHTML))
}

func handleStreamTestPage(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(streamViewerTestHTML))
}

func handleWebRTCStreamSession(c *gin.Context) {
	connectionID := uuid.New().String()
	scopedLogger := webrtcLogger.With().Str("component", "stream-session").Str("connectionID", connectionID).Logger()

	var req WebRTCSessionRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	session, err := newSession(SessionConfig{
		MDNSMode: config.NetworkConfig.MDNSMode.String,
		Logger:   &scopedLogger,
		OnClosed: func(session *Session) {
			unregisterStreamViewerSession(session)
			scopedLogger.Info().Int("activeStreamViewerSessions", getActiveStreamViewerSessions()).Msg("stream viewer session closed")
		},
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	sd, err := session.ExchangeOffer(req.Sd)
	if err != nil {
		_ = session.peerConnection.Close()
		c.JSON(http.StatusInternalServerError, gin.H{"error": err})
		return
	}

	registerStreamViewerSession(session)
	scopedLogger.Info().Int("activeStreamViewerSessions", getActiveStreamViewerSessions()).Msg("stream viewer session created")
	c.JSON(http.StatusOK, gin.H{"sd": sd})
}

func handleRawVideoStream(c *gin.Context) {
	subscriber := subscribeVideoStream()
	defer unsubscribeVideoStream(subscriber)

	c.Header("Content-Type", "video/h264")
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming not supported"})
		return
	}

	for {
		select {
		case frame := <-subscriber:
			if _, err := c.Writer.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-c.Request.Context().Done():
			return
		}
	}
}
