"use client"

import { useState, type ComponentProps } from "react"
import { ArrowRight, Check } from "lucide-react"
import { Button } from "@/components/ui/button"
import type { AIChatAnswer, AIChatInterview, AIChatQuestion, AIChatStepAnswers } from "@/lib/api"

export function selectChoice(question: AIChatQuestion, current: AIChatAnswer, choice: string): AIChatAnswer {
  if (question.kind === "single") return { questionId: question.id, choices: [choice] }
  return { ...current, choices: current.choices.includes(choice) ? current.choices.filter((item) => item !== choice) : [...current.choices, choice] }
}
export function enterOther(question: AIChatQuestion, current: AIChatAnswer, other: string): AIChatAnswer {
  return { ...current, choices: question.kind === "single" ? [] : current.choices, other }
}
export function stepComplete(questions: AIChatQuestion[], answers: ReadonlyMap<string, AIChatAnswer>): boolean {
  return questions.every((question) => {
    const answer = answers.get(question.id)
    return !!answer && (answer.choices.length > 0 || !!answer.other?.trim())
  })
}

// Native controls render as solid white discs in dark mode; these match lite-01.
function Choice({ type, className = "", ...props }: { type: "radio" | "checkbox" } & Omit<ComponentProps<"input">, "type">) {
  if (type === "radio") return <input type="radio" className={`mt-0.5 size-4 shrink-0 appearance-none rounded-full border border-muted-foreground/60 outline-none checked:border-[5px] checked:border-foreground checked:bg-background ${className}`} {...props} />
  return <span className={`relative mt-0.5 flex size-4 shrink-0 ${className}`}>
    <input type="checkbox" className="peer size-4 appearance-none rounded-[4px] border border-muted-foreground/60 outline-none checked:border-foreground checked:bg-foreground" {...props} />
    <Check strokeWidth={3} className="pointer-events-none absolute inset-0 m-auto hidden size-3 text-background peer-checked:block" />
  </span>
}

export function InterviewCard({ interview, disabled, onAnswer }: {
  interview: AIChatInterview
  disabled: boolean
  onAnswer?: (questionSetId: string, steps: AIChatStepAnswers[]) => void
}) {
  const [step, setStep] = useState(0)
  const [answers, setAnswers] = useState(new Map<string, AIChatAnswer>())
  const [completed, setCompleted] = useState<AIChatStepAnswers[]>([])
  const [otherSelected, setOtherSelected] = useState(new Map<string, boolean>())
  const questions = interview.steps[step].questions
  const inactive = disabled || !onAnswer
  const answerFor = (question: AIChatQuestion): AIChatAnswer => answers.get(question.id) ?? { questionId: question.id, choices: [] }
  const update = (answer: AIChatAnswer) => setAnswers((previous) => new Map(previous).set(answer.questionId, answer))
  const advance = (skipped: boolean) => {
    if (inactive || (!skipped && !stepComplete(questions, answers))) return
    const steps = [...completed, { skipped, answers: skipped ? [] : questions.map(answerFor) }]
    if (step === interview.steps.length - 1) onAnswer?.(interview.questionSetId, steps)
    else { setCompleted(steps); setStep(step + 1) }
  }
  const firstNumber = interview.steps.slice(0, step).reduce((count, item) => count + item.questions.length, 0)

  return <div className={`my-3 rounded-xl border bg-card p-4 ${inactive ? "opacity-60" : ""}`}>
    <div className="flex items-center justify-between gap-3 text-[11px] text-muted-foreground">
      <h3 className="font-semibold uppercase tracking-wide">{interview.title}</h3><span>Step {step + 1} of {interview.steps.length}</span>
    </div>
    <div className="mt-3 flex gap-1" aria-hidden="true">{interview.steps.map((_, index) => <div key={index} className={`h-0.5 flex-1 rounded-full ${index <= step ? "bg-foreground" : "bg-border"}`} />)}</div>
    <div className="mt-4 space-y-4">{questions.map((question, index) => {
      const answer = answerFor(question)
      const other = !!otherSelected.get(question.id) || !!answer.other
      const inputType = question.kind === "single" ? "radio" : "checkbox"
      const name = `${interview.questionSetId}-${question.id}`
      const tileClass = (selected: boolean) => `flex items-start gap-2.5 rounded-lg border p-2.5 has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring ${selected ? "border-foreground bg-muted/50" : "border-border"}`
      return <fieldset key={question.id} disabled={inactive}>
        <legend className="mb-2 flex w-full items-baseline gap-2 text-[13px]"><span className="text-xs text-muted-foreground">{firstNumber + index + 1}</span><span className="flex-1 font-medium">{question.prompt}</span><span className="shrink-0 text-xs text-muted-foreground">{question.kind === "single" ? "Pick one" : "Pick any"}</span></legend>
        <div className="grid gap-2 sm:grid-cols-2">
          {question.options.map((option, optionIndex) => <label key={optionIndex} className={tileClass(answer.choices.includes(option.label))}>
            <Choice type={inputType} name={name} checked={answer.choices.includes(option.label)} onChange={() => {
              update(selectChoice(question, answer, option.label))
              if (question.kind === "single") setOtherSelected((previous) => new Map(previous).set(question.id, false))
            }} />
            <span className="text-[13px]"><span className="block font-medium">{option.label}</span>{option.detail && <span className="mt-1 block text-xs text-muted-foreground">{option.detail}</span>}</span>
          </label>)}
          <div className={tileClass(other)}>
            <Choice aria-label={`Other: ${question.prompt}`} className="mt-1" type={inputType} name={name} checked={other} onChange={() => {
              const selected = inputType === "radio" || !other
              setOtherSelected((previous) => new Map(previous).set(question.id, selected))
              update(enterOther(question, answer, selected ? answer.other ?? "" : ""))
            }} />
            <label className="min-w-0 flex-1"><span className="sr-only">Your other answer: {question.prompt}</span>
              <input type="text" value={answer.other ?? ""} placeholder="Type your answer…" onChange={(event) => {
                setOtherSelected((previous) => new Map(previous).set(question.id, true))
                update(enterOther(question, answer, Array.from(event.target.value).slice(0, 300).join("")))
              }} className="w-full rounded border bg-background px-2 py-1 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring" />
            </label>
          </div>
        </div>
      </fieldset>
    })}</div>
    <div className="mt-5 flex flex-wrap items-center justify-between gap-3 border-t pt-3">
      <span className="text-xs text-muted-foreground">Or just type in the composer.</span>
      <div className="ml-auto flex gap-2"><Button variant="ghost" size="sm" className="font-semibold" disabled={inactive} onClick={() => advance(true)}>Skip</Button>
        <Button size="sm" disabled={inactive || !stepComplete(questions, answers)} onClick={() => advance(false)}><ArrowRight className="size-3.5" />Continue</Button></div>
    </div>
  </div>
}
