import Foundation
import os

/// One WebSocket to knockd. Control messages are JSON text frames; terminal
/// bytes are binary frames. Reconnects with backoff until stopped.
@MainActor
final class RelayClient {
    enum State: Equatable {
        case idle, connecting, connected, offline(String)
    }

    var onState: (State) -> Void = { _ in }
    var onWelcome: (Welcome) -> Void = { _ in }
    var onEvent: (Envelope) -> Void = { _ in }
    var onData: (Data) -> Void = { _ in }
    /// Builds the hello for each (re)connect, refreshing the token if needed.
    var makeHello: () async throws -> Hello = { throw WireError(code: "unconfigured", message: nil) }

    private let log = Logger(subsystem: "io.github.idestis.knock", category: "relay")
    private var url: URL?
    private var task: URLSessionWebSocketTask?
    private var generation = 0
    private var waiters: [String: CheckedContinuation<Envelope, Error>] = [:]
    private var nextId = 0
    private var backoff: Double = 1
    private var outbox: AsyncStream<URLSessionWebSocketTask.Message>.Continuation?
    private(set) var state: State = .idle {
        didSet { if state != oldValue { onState(state) } }
    }

    static func wsURL(_ relay: URL) -> URL {
        var c = URLComponents(url: relay, resolvingAgainstBaseURL: false)!
        c.scheme = c.scheme == "https" ? "wss" : "ws"
        c.path = (c.path.hasSuffix("/") ? String(c.path.dropLast()) : c.path) + "/v1/ws"
        return c.url!
    }

    func start(relay: URL) {
        url = relay
        connect()
    }

    func stop() {
        url = nil
        teardown(reason: nil)
        state = .idle
    }

    /// Drop the socket and connect again now (e.g. after signing in).
    func reconnect() {
        guard url != nil else { return }
        teardown(reason: nil)
        backoff = 1
        connect()
    }

    private func teardown(reason: String?) {
        generation += 1
        task?.cancel(with: .normalClosure, reason: nil)
        task = nil
        outbox?.finish()
        outbox = nil
        let pending = waiters
        waiters = [:]
        for (_, w) in pending { w.resume(throwing: WireError(code: "disconnected", message: reason ?? "Connection to the relay dropped.")) }
    }

    private func connect() {
        guard let url else { return }
        generation += 1
        let gen = generation
        state = .connecting
        var req = URLRequest(url: Self.wsURL(url))
        req.setValue("Knock-mac/\(AppInfo.version)", forHTTPHeaderField: "User-Agent")
        let t = URLSession.shared.webSocketTask(with: req)
        t.maximumMessageSize = 1 << 20
        task = t
        let (stream, cont) = AsyncStream<URLSessionWebSocketTask.Message>.makeStream(bufferingPolicy: .unbounded)
        outbox = cont
        t.resume()

        // Ordered writer.
        Task { [weak self] in
            for await m in stream {
                do { try await t.send(m) } catch {
                    self?.lost(gen, error)
                    return
                }
            }
        }
        // Reader.
        Task { [weak self] in
            while true {
                do {
                    let m = try await t.receive()
                    guard let self, gen == self.generation else { return }
                    switch m {
                    case .data(let d): self.onData(d)
                    case .string(let s): self.handle(Data(s.utf8))
                    @unknown default: break
                    }
                } catch {
                    self?.lost(gen, error)
                    return
                }
            }
        }
        // Hello, then keepalive.
        Task { [weak self] in
            guard let self else { return }
            do {
                let hello = try await self.makeHello()
                let env = try await self.request(MessageType.hello, hello)
                guard gen == self.generation else { return }
                let welcome = try env.decode(Welcome.self)
                self.backoff = 1
                self.state = .connected
                self.onWelcome(welcome)
            } catch let e as WireError {
                self.log.error("hello failed: \(e.code, privacy: .public)")
                self.lost(gen, e)
                return
            } catch {
                self.lost(gen, error)
                return
            }
            while gen == self.generation {
                try? await Task.sleep(for: .seconds(20))
                guard gen == self.generation else { return }
                t.sendPing { _ in }
            }
        }
    }

    private func lost(_ gen: Int, _ error: Error) {
        guard gen == generation else { return }
        let msg = (error as? WireError)?.text ?? "Can't reach the relay."
        teardown(reason: msg)
        state = .offline(msg)
        if let e = error as? WireError, e.code == WireError.unauthorised || e.code == WireError.anonymousNotAllowed {
            onEvent(Envelope(type: MessageType.error, id: nil, data: nil, error: e))
        }
        guard url != nil else { return }
        let delay = backoff
        backoff = min(backoff * 2, 30)
        let next = generation
        Task { [weak self] in
            try? await Task.sleep(for: .seconds(delay))
            guard let self, self.generation == next, self.url != nil else { return }
            self.connect()
        }
    }

    private func handle(_ raw: Data) {
        guard let env = try? Envelope.parse(raw) else { return }
        if let id = env.id, let w = waiters.removeValue(forKey: id) {
            if let e = env.error { w.resume(throwing: e) } else { w.resume(returning: env) }
            // session.start answering a knock is also an event.
            if env.type != MessageType.sessionStart { return }
        }
        if env.type == MessageType.relayDraining {
            log.info("relay draining, reconnecting")
            reconnect()
            return
        }
        onEvent(env)
    }

    // MARK: Sending

    func send(_ type: String, _ payload: (some Encodable)? = Empty?.none) {
        guard let s = try? Envelope.encode(type: type, id: nil, data: payload) else { return }
        outbox?.yield(.string(s))
    }

    func sendData(_ d: Data) {
        outbox?.yield(.data(d))
    }

    func request(_ type: String, _ payload: (some Encodable)? = Empty?.none, timeout: Double = 15) async throws -> Envelope {
        guard let outbox else { throw WireError(code: "disconnected", message: "Not connected to the relay.") }
        nextId += 1
        let id = String(nextId)
        let s = try Envelope.encode(type: type, id: id, data: payload)
        return try await withCheckedThrowingContinuation { cont in
            waiters[id] = cont
            outbox.yield(.string(s))
            Task { [weak self] in
                try? await Task.sleep(for: .seconds(timeout))
                if let w = self?.waiters.removeValue(forKey: id) {
                    w.resume(throwing: WireError(code: "timeout", message: "The relay didn't answer."))
                }
            }
        }
    }

    func request<T: Decodable>(_ type: String, _ payload: some Encodable, as: T.Type) async throws -> T {
        try await request(type, payload).decode(T.self)
    }
}

enum AppInfo {
    static var version: String { Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "dev" }
    static var build: String { Bundle.main.infoDictionary?["CFBundleVersion"] as? String ?? "0" }
    static var deviceName: String {
        #if DEBUG
        if let demo = ProcessInfo.processInfo.environment["KNOCK_DEMO_DEVICE"] { return demo }
        #endif
        let name = Host.current().localizedName ?? ProcessInfo.processInfo.hostName
        return name.lowercased().replacingOccurrences(of: "’", with: "").replacingOccurrences(of: "'", with: "")
            .split(whereSeparator: { !$0.isLetter && !$0.isNumber }).joined(separator: "-")
    }
}
