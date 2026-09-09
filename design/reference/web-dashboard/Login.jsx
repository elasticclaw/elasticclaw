const { Button, Input, Icon } = window.ElasticClawDesignSystem_0b1de0

function LoginScreen({ onSignIn }) {
  const [pw, setPw] = React.useState('')
  return (
    <div style={{ display: 'flex', height: '100%', alignItems: 'center', justifyContent: 'center', background: 'var(--background)' }}>
      <div style={{ width: '100%', maxWidth: 384, display: 'flex', flexDirection: 'column', gap: 24, padding: 32, border: '1px solid var(--border)', borderRadius: 'var(--radius-xl)', background: 'var(--card)' }}>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 4, textAlign: 'center' }}>
          <h1 style={{ margin: 0, fontSize: 'var(--text-2xl)', fontWeight: 'var(--weight-semibold)', letterSpacing: 'var(--tracking-tight)' }}>ElasticClaw</h1>
          <p style={{ margin: 0, fontSize: 'var(--text-sm)', color: 'var(--muted-foreground)' }}>Sign in to continue</p>
        </div>
        <button
          type="button"
          onClick={onSignIn}
          style={{ display: 'flex', width: '100%', alignItems: 'center', justifyContent: 'center', gap: 8, padding: '8px 12px', borderRadius: 'var(--radius-md)', border: '1px solid var(--border)', background: 'var(--background)', color: 'var(--foreground)', fontFamily: 'var(--font-sans)', fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)', cursor: 'pointer' }}
        >
          <svg viewBox="0 0 16 16" width="16" height="16" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8z" /></svg>
          Sign in with GitHub
        </button>
        <div style={{ position: 'relative', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
          <span style={{ position: 'absolute', inset: 0, top: '50%', borderTop: '1px solid var(--border)' }} />
          <span style={{ position: 'relative', padding: '0 8px', background: 'var(--card)', fontSize: 'var(--text-12)', textTransform: 'uppercase', color: 'var(--muted-foreground)' }}>or</span>
        </div>
        <form onSubmit={(e) => { e.preventDefault(); onSignIn() }} style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <Input type="password" placeholder="Access token" value={pw} onChange={(e) => setPw(e.target.value)} />
          <Button type="submit" style={{ width: '100%' }} disabled={!pw}>Sign in</Button>
        </form>
      </div>
    </div>
  )
}

Object.assign(window, { LoginScreen })
