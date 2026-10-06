import ActivityKit
import AppIntents
import SwiftUI
import WidgetKit

/// The broadcast's Live Activity (docs/70 B5–B7, D15): the code, the
/// audience and a way to stop, where iOS puts them while you're in a game.
/// The app sends the state; this only draws it.
@main
struct GawkLiveActivityBundle: WidgetBundle {
    var body: some Widget {
        GawkLiveActivity()
    }
}

struct GawkLiveActivity: Widget {
    var body: some WidgetConfiguration {
        ActivityConfiguration(for: GawkActivityAttributes.self) { context in
            LockScreenView(context: context)
                .activityBackgroundTint(Theme.bg)
                .activitySystemActionForegroundColor(Theme.text)
        } dynamicIsland: { context in
            DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    LiveLabel(context: context, timer: true)
                }
                DynamicIslandExpandedRegion(.trailing) {
                    // Clear of the Island's rounded corner.
                    Viewers(count: context.state.viewers)
                        .padding(.trailing, 8)
                        .frame(maxHeight: .infinity, alignment: .center)
                }
                DynamicIslandExpandedRegion(.bottom) {
                    VStack(spacing: 10) {
                        Text(context.attributes.code)
                            .font(.system(size: 26, weight: .semibold, design: .monospaced))
                            .tracking(2)
                            .foregroundStyle(Theme.text)
                            .accessibilityLabel("Code \(context.attributes.code)")
                        Actions(link: context.attributes.link)
                    }
                }
            } compactLeading: {
                HStack(spacing: 4) {
                    Circle().fill(Theme.live).frame(width: 7, height: 7)
                    Text("LIVE").font(.caption2.weight(.bold)).foregroundStyle(Theme.liveText)
                }
            } compactTrailing: {
                Viewers(count: context.state.viewers)
            } minimal: {
                Circle().fill(Theme.live).frame(width: 8, height: 8)
                    .accessibilityLabel("Live")
            }
        }
    }
}

/// The Lock Screen (B5).
private struct LockScreenView: View {
    let context: ActivityViewContext<GawkActivityAttributes>

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 8) {
                // The generated app icon (K13), never a redrawn mark.
                Image("Mark")
                    .resizable()
                    .frame(width: 22, height: 22)
                    .clipShape(.rect(cornerRadius: 5, style: .continuous))
                    .accessibilityHidden(true)
                Text("gawk").font(.subheadline.weight(.semibold)).foregroundStyle(Theme.text)
                Spacer()
                LiveLabel(context: context, timer: true)
                Viewers(count: context.state.viewers)
            }
            VStack(alignment: .leading, spacing: 2) {
                Text("Your code")
                    .font(.caption.weight(.semibold))
                    .textCase(.uppercase)
                    .tracking(1.2)
                    .foregroundStyle(Theme.muted)
                Text(context.attributes.code)
                    .font(.system(size: 30, weight: .semibold, design: .monospaced))
                    .tracking(3)
                    .foregroundStyle(Theme.text)
                    .accessibilityLabel("Code \(context.attributes.code)")
            }
            Actions(link: context.attributes.link)
        }
        .padding(16)
    }
}

/// LIVE (or RECONNECTING) with the time live.
private struct LiveLabel: View {
    let context: ActivityViewContext<GawkActivityAttributes>
    var timer: Bool

    var body: some View {
        let reconnecting = context.state.reconnecting
        HStack(spacing: 5) {
            Text(reconnecting ? "RECONNECTING" : "LIVE").fontWeight(.bold)
            if timer, !reconnecting {
                Text(timerInterval: context.attributes.since...Date.distantFuture, countsDown: false)
                    .monospacedDigit()
                    .frame(width: 44)
            }
        }
        .fixedSize()
        .font(.caption)
        .foregroundStyle(Theme.text)
        .padding(.horizontal, 8)
        .frame(height: 24)
        .background(reconnecting ? Theme.warn : Theme.live, in: .capsule)
    }
}

private struct Viewers: View {
    let count: UInt32

    var body: some View {
        HStack(spacing: 3) {
            Image(systemName: "eye")
            Text("\(count)").monospacedDigit()
        }
        .font(.caption.weight(.semibold))
        .foregroundStyle(Theme.text)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(count) watching")
    }
}

/// Copy link (glass) and End (danger outline), as `LiveActivityIntent`s
/// that run in the app.
private struct Actions: View {
    let link: String

    var body: some View {
        HStack(spacing: 10) {
            Button(intent: CopyLinkIntent(link: link)) {
                Label("Copy link", systemImage: "link")
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Theme.text)
                    .frame(maxWidth: .infinity, minHeight: 36)
                    .background(Theme.text.opacity(0.12), in: .capsule)
            }
            .buttonStyle(.plain)
            Button(intent: EndBroadcastIntent()) {
                Text("End")
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Theme.dangerText)
                    .frame(maxWidth: .infinity, minHeight: 36)
                    .overlay(Capsule().strokeBorder(Theme.dangerLine, lineWidth: 1.5))
            }
            .buttonStyle(.plain)
        }
    }
}
