import SwiftUI

@main
struct QueqiaoApp: App {
    @StateObject private var model = TunnelModel()

    var body: some Scene {
        WindowGroup {
            ContentView()
                .environmentObject(model)
        }
    }
}
