interface NimbusBlobsProps {
  intensity?: number
}

const BLOBS = [
  { color: 'var(--blob-peach)',      opacity: 0.22, left: '-15%', top: '-20%', width: '60%',  height: '55%' },
  { color: 'var(--blob-pink)',       opacity: 0.18, left: '60%',  top: '-10%', width: '55%',  height: '50%' },
  { color: 'var(--blob-lavender)',   opacity: 0.14, left: '-10%', top: '55%',  width: '55%',  height: '55%' },
  { color: 'var(--blob-soft-peach)', opacity: 0.16, left: '55%',  top: '50%',  width: '60%',  height: '60%' },
]

export function NimbusBlobs({ intensity = 1 }: NimbusBlobsProps) {
  return (
    <div
      aria-hidden="true"
      style={{ position: 'absolute', inset: 0, pointerEvents: 'none', overflow: 'hidden' }}
    >
      {BLOBS.map((blob, i) => (
        <div
          key={i}
          style={{
            position: 'absolute',
            left: blob.left,
            top: blob.top,
            width: blob.width,
            height: blob.height,
            background: blob.color,
            opacity: blob.opacity * intensity,
            borderRadius: '50%',
            filter: 'blur(80px)',
          }}
        />
      ))}
      <div
        style={{
          position: 'absolute',
          inset: 0,
          backgroundImage:
            'radial-gradient(circle at 1px 1px, rgba(20,18,28,0.025) 1px, transparent 0)',
          backgroundSize: '3px 3px',
          opacity: 0.6,
        }}
      />
    </div>
  )
}
