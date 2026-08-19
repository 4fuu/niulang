import Foundation

extension TunnelModel {
    var canTestProfiles: Bool {
        !isBusy && !profileProbeStates.values.contains(.testing)
    }

    func testProfile(id: String) async {
        guard canTestProfiles else { return }
        profileProbeStates[id] = .testing
        let outcome = await Task.detached(priority: .userInitiated) {
            probeStoredProfile(id: id)
        }.value
        profileProbeStates[id] = outcome.state
    }

    func testAllProfiles() async {
        guard canTestProfiles else { return }
        let profileIDs = profiles.map(\.id)
        guard !profileIDs.isEmpty else { return }
        for id in profileIDs {
            profileProbeStates[id] = .testing
        }
        await withTaskGroup(of: ProfileProbeOutcome.self) { group in
            var iterator = profileIDs.makeIterator()
            for _ in 0..<min(4, profileIDs.count) {
                guard let id = iterator.next() else { break }
                group.addTask(priority: .userInitiated) { probeStoredProfile(id: id) }
            }
            while let outcome = await group.next() {
                profileProbeStates[outcome.profileID] = outcome.state
                if let id = iterator.next() {
                    group.addTask(priority: .userInitiated) { probeStoredProfile(id: id) }
                }
            }
        }
    }
}
