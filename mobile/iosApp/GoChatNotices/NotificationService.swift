import Security
import UserNotifications

/// Sees each push before it shows. Once the app is signed out, a push is
/// for a session signed out of offline, which the server pushes to until
/// the app can tell it: that push shows neither who wrote, nor where, nor
/// what.
final class NotificationService: UNNotificationServiceExtension {
    override func didReceive(_ request: UNNotificationRequest, withContentHandler contentHandler: @escaping (UNNotificationContent) -> Void) {
        switch NotificationService.session() {
        case true?:
            contentHandler(request.content)
        case false?:
            let content = UNMutableNotificationContent()
            content.title = "Signed out"
            content.body = "Open GoChat to finish signing out."
            content.threadIdentifier = "signed-out"
            contentHandler(content)
        case nil:
            // The Keychain cannot say, as before the phone's first unlock.
            let content = UNMutableNotificationContent()
            content.body = "New message"
            content.sound = .default
            contentHandler(content)
        }
    }

    /// Whether the app holds a session: its token is in the Keychain, where
    /// the app's KeychainStore keeps it, in the group the two share. Nil
    /// when the Keychain cannot say.
    static func session() -> Bool? {
        let query: [CFString: Any] = [
            kSecClass: kSecClassGenericPassword,
            kSecAttrService: "io.github.ssenerg.gochat",
            kSecAttrAccount: "token",
        ]
        switch SecItemCopyMatching(query as CFDictionary, nil) {
        case errSecSuccess: return true
        case errSecItemNotFound: return false
        default: return nil
        }
    }
}
