// Renders AppIcon PNGs from knock-mark.svg: full-bleed white tile, blue K.
// Usage: swift scripts/make-icons.swift scripts/knock-mark.svg Resources/Assets.xcassets/AppIcon.appiconset
import AppKit

let args = CommandLine.arguments
guard args.count == 3, let mark = NSImage(contentsOfFile: args[1]) else { fatalError("usage: make-icons <svg> <out>") }
let out = URL(fileURLWithPath: args[2])
var images: [[String: String]] = []
for base in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let px = base * scale
        let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px, bitsPerSample: 8, samplesPerPixel: 4,
                                   hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
        let s = CGFloat(px)
        NSColor.white.setFill()
        NSRect(x: 0, y: 0, width: s, height: s).fill()
        let w = s * 0.56, h = w * mark.size.height / mark.size.width
        mark.draw(in: NSRect(x: (s - w) / 2 + s * 0.01, y: (s - h) / 2, width: w, height: h))
        NSGraphicsContext.restoreGraphicsState()
        let name = "icon_\(base)x\(base)\(scale == 2 ? "@2x" : "").png"
        try! rep.representation(using: .png, properties: [:])!.write(to: out.appendingPathComponent(name))
        images.append(["filename": name, "idiom": "mac", "scale": "\(scale)x", "size": "\(base)x\(base)"])
    }
}
let json: [String: Any] = ["images": images, "info": ["author": "xcode", "version": 1]]
try! JSONSerialization.data(withJSONObject: json, options: [.prettyPrinted]).write(to: out.appendingPathComponent("Contents.json"))
print("wrote \(images.count) icons")
