// Background renders the four animated peach/pink/lavender blobs and the
// film grain overlay used throughout the Nimbus UI. Mount once at the App
// root — it positions itself with `fixed inset-0` and lives behind content.
export default function Background() {
  return (
    <>
      <div className="blobs">
        <div
          className="blob animate-drift"
          style={{
            width: 720,
            height: 720,
            background: 'rgba(248, 175, 130, 0.22)',
            top: -180,
            left: -160,
          }}
        />
        <div
          className="blob animate-drift"
          style={{
            width: 620,
            height: 620,
            background: 'rgba(244, 150, 180, 0.18)',
            top: '20%',
            right: -120,
            animationDelay: '-7s',
          }}
        />
        <div
          className="blob animate-drift"
          style={{
            width: 780,
            height: 780,
            background: 'rgba(210, 170, 240, 0.14)',
            bottom: -200,
            left: '30%',
            animationDelay: '-12s',
          }}
        />
        <div
          className="blob animate-drift"
          style={{
            width: 480,
            height: 480,
            background: 'rgba(248, 180, 150, 0.16)',
            top: '55%',
            left: -100,
            animationDelay: '-18s',
          }}
        />
      </div>
      <div className="grain" />
    </>
  )
}
