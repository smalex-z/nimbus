// ApiDocs renders the OpenAPI / SwaggerUI surface in an iframe so the
// docs live inside Nimbus's chrome (sidebar + header + back-button) rather
// than navigating away to the bare /api/docs/ surface. The actual UI is
// served by httpSwagger at /api/docs/ — we just frame it here.
//
// Sized via calc(100vh - 280px) — the navbar (sticky, ~73px) + page
// padding (py-12 + bottom buffer) + the Infrastructure header eat the
// rest. Adjust if the layout shell changes.
export default function ApiDocs() {
  return (
    <iframe
      src="/api/docs/"
      title="Nimbus API documentation"
      className="w-full glass"
      style={{
        minHeight: 'calc(100vh - 280px)',
        height: 'calc(100vh - 280px)',
        border: 'none',
        // The glass class adds rounded corners + the subtle border;
        // overflow:hidden clips the iframe corners to the rounded edge.
        overflow: 'hidden',
      }}
    />
  )
}
