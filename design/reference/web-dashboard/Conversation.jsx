const DS = window.ElasticClawDesignSystem_0b1de0
const { Button, Icon, StatusDot, Badge, ContextProgressBar, NowStrip, TimelineToolbar, TurnCard, StepRow, MessageBubble, Composer, TypingDots } = DS

function ConversationScreen({ agent, onBack, onSend, onKill }) {
  const [density, setDensity] = React.useState('conversation')
  const failed = agent.steps.filter((s) => s.status === 'failed').length
  return (
    <div style={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '10px 24px', borderBottom: '1px solid var(--border)' }}>
        <Button variant="ghost" size="icon-sm" title="Back to board" onClick={onBack}><Icon name="ChevronLeft" size={16} /></Button>
        <StatusDot status={agent.status} isStreaming={agent.isStreaming} />
        <span style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-sm)', fontWeight: 'var(--weight-medium)', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{agent.name}</span>
        <span style={{ fontSize: 'var(--text-12)', color: 'var(--muted-foreground)' }}>{agent.template}</span>
        <div style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 8 }}>
          <ContextProgressBar usage={agent.contextUsage} size="lg" />
          <Button variant="ghost" size="icon-sm" title="Terminal"><Icon name="TerminalSquare" size={16} /></Button>
          <Button variant="ghost" size="icon-sm" title="Copy transcript"><Icon name="ClipboardCopy" size={16} /></Button>
          <Button variant="destructive" size="sm" onClick={() => onKill(agent.id)}><Icon name="Trash2" size={12} />Kill</Button>
        </div>
      </div>

      {agent.now && <NowStrip {...agent.now} />}
      <TimelineToolbar density={density} onDensityChange={setDensity} turns={2} toolCalls={agent.steps.length} failures={failed} />

      <div style={{ flex: 1, minHeight: 0, overflowY: 'auto', padding: '16px 24px', display: 'flex', flexDirection: 'column', gap: 12 }}>
        {/* Chronological, because the thread has more than one human in it: a
            teammate's reply only makes sense next to what it answered. The turn
            card sits where the agent's work happened — after its first reply. */}
        {agent.messages.map((m, i) => (
          <React.Fragment key={m.id}>
            <window.Bubble role={m.role} time={m.time} accent={agent.accent} {...window.messageAuthor(m, agent)}>{m.content}</window.Bubble>
            {m.role === 'claw' && agent.steps.length > 0 && i === agent.messages.findIndex((x) => x.role === 'claw') && (
              <TurnCard
                label={agent.status === 'error' ? 'Provisioning attempt' : 'Reproduce and patch the flake'}
                status={agent.isStreaming ? 'running' : failed ? 'failed' : 'ok'}
                stepCount={agent.steps.length} failedCount={failed} durationMs={94000} clock="14:07"
              >
                {agent.steps.map((s, si) => <StepRow key={si} {...s} />)}
              </TurnCard>
            )}
          </React.Fragment>
        ))}
        {agent.isStreaming && <TypingDots />}
      </div>

      <Composer onSend={(t) => onSend(agent.id, t)} disabled={agent.status === 'error'} placeholder={agent.status === 'error' ? 'Provisioning failed' : 'Message agent, /stop, or attach files'} />
    </div>
  )
}

Object.assign(window, { ConversationScreen })
