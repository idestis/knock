import Foundation
import UserNotifications

/// Native macOS notifications for knocks, with "Let in" and "Not now" actions.
/// Letting someone in always takes an explicit click; there is no default action.
@MainActor
final class Notifier: NSObject {
    static let category = "KNOCK"
    static let letIn = "LET_IN"
    static let notNow = "NOT_NOW"

    private weak var model: AppModel?
    private var pending: [String: KnockIncoming] = [:]

    func setUp(model: AppModel) {
        self.model = model
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        let letIn = UNNotificationAction(identifier: Self.letIn, title: "Let in", options: [.authenticationRequired])
        let notNow = UNNotificationAction(identifier: Self.notNow, title: "Not now", options: [.destructive])
        center.setNotificationCategories([
            UNNotificationCategory(identifier: Self.category, actions: [letIn, notNow], intentIdentifiers: [],
                                   options: [.customDismissAction]),
        ])
        // Permission is asked in the onboarding wizard, not on every launch.
    }

    func post(_ k: KnockIncoming) {
        pending[k.knockId] = k
        let c = UNMutableNotificationContent()
        c.title = "Knock, knock."
        let verified = k.from.verifiedLine.map { " (\($0))" } ?? " (not verified)"
        c.body = "\(k.from.name)\(verified) wants to come in from \(k.device)."
        c.categoryIdentifier = Self.category
        c.sound = .default
        c.interruptionLevel = .timeSensitive
        c.userInfo = ["knock_id": k.knockId]
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: k.knockId, content: c, trigger: nil))
    }

    /// A plain heads-up with no actions, such as "Knock made you a new code".
    func postNotice(id: String, title: String, body: String) {
        let c = UNMutableNotificationContent()
        c.title = title
        c.body = body
        c.sound = .default
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: c, trigger: nil))
    }

    func remove(_ knockId: String) {
        pending[knockId] = nil
        UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: [knockId])
        UNUserNotificationCenter.current().removePendingNotificationRequests(withIdentifiers: [knockId])
    }

    /// A new code or a closed door: no old knock's "Let in" should be left lying around.
    func removeAll() {
        let ids = Array(pending.keys)
        pending.removeAll()
        guard !ids.isEmpty else { return }
        UNUserNotificationCenter.current().removeDeliveredNotifications(withIdentifiers: ids)
        UNUserNotificationCenter.current().removePendingNotificationRequests(withIdentifiers: ids)
    }
}

extension Notifier: UNUserNotificationCenterDelegate {
    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async
        -> UNNotificationPresentationOptions {
        [.banner, .sound, .list]
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        let id = response.notification.request.identifier
        let action = response.actionIdentifier
        await MainActor.run {
            guard let model, let k = pending[id] ?? model.incoming.first(where: { $0.knockId == id }) else { return }
            switch action {
            case Self.letIn: model.answer(k, allow: true)
            case Self.notNow, UNNotificationDismissActionIdentifier: model.answer(k, allow: false)
            default: model.showMainWindow() // clicked the body: show the prompt, decide there
            }
        }
    }
}
