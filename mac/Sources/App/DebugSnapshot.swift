#if DEBUG
import AppKit
import SwiftUI

/// Debug builds only: KNOCK_SNAPSHOT=/dir renders key screens with ImageRenderer
/// every few seconds, so layout can be checked without screen recording.
/// Liquid Glass renders as a plain fill in these images.
enum DebugSnapshot {
    @MainActor static func startIfRequested() {
        guard let dir = ProcessInfo.processInfo.environment["KNOCK_SNAPSHOT"] else { return }
        let model = AppModel.shared
        let sample = KnockIncoming(knockId: "k_sample",
                                   from: Identity(id: "github:1", name: "Sam Carter", email: "sam@example.com", provider: "github", verified: true),
                                   device: "sam-mbp", expiresIn: 60)
        Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { _ in
            MainActor.assumeIsolated {
                write(HomeView().environment(model).frame(width: 980, height: 660), "home", dir)
                write(ZStack {
                    KnockBackdrop()
                    HStack(spacing: 0) {
                        SidebarView().frame(width: 300).frame(maxHeight: .infinity)
                            .glassEffect(.regular.tint(Color.white.opacity(0.35)), in: .rect(cornerRadius: 26)).padding(10)
                        MainColumn().padding(.leading, 22).padding(.trailing, 26).padding(.top, 26).padding(.bottom, 22)
                    }
                }.environment(model).frame(width: 980, height: 660), "main", dir)
                write(ZStack { KnockBackdrop(); KnockAlertCard(knock: sample, onAllow: {}, onDeny: {}) }.frame(width: 600, height: 520), "alert", dir)
                write(KnockPromptPanelView(knock: sample, notch: true, notchWidth: 200, notchHeight: 32, onAllow: {}, onDeny: {})
                    .frame(width: 460, height: 168), "notch", dir)
                write(ZStack { KnockBackdrop(); OnboardingView(step: 4) }.environment(model).frame(width: 980, height: 660), "onboarding-disk", dir)
            }
        }
    }

    @MainActor private static func write(_ view: some View, _ name: String, _ dir: String) {
        let r = ImageRenderer(content: view)
        r.scale = 2
        guard let img = r.nsImage, let tiff = img.tiffRepresentation, let rep = NSBitmapImageRep(data: tiff),
              let png = rep.representation(using: .png, properties: [:]) else { return }
        try? png.write(to: URL(fileURLWithPath: dir).appendingPathComponent("\(name).png"))
    }
}
#endif
