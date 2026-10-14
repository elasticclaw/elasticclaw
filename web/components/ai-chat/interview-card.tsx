"use client"

import { useState } from "react"
import { ArrowRight } from "lucide-react"
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
    <div className="flex items-center justify-between gap-3 text-xs text-muted-foreground">
      <h3 className="font-semibold uppercase tracking-wide">{interview.title}</h3><span>Step {step + 1} of {interview.steps.length}</span>
    </div>
    <div className="mt-3 flex gap-1" aria-hidden="true">{interview.steps.map((_, index) => <div key={index} className={`h-0.5 flex-1 rounded-full ${index <= step ? "bg-foreground" : "bg-border"}`} />)}</div>
    <div className="mt-4 space-y-5">{questions.map((question, index) => {
      const answer = answerFor(question)
      const other = !!otherSelected.get(question.id) || !!answer.other
      const inputType = question.kind === "single" ? "radio" : "checkbox"
      const name = `${interview.questionSetId}-${question.id}`
      const tileClass = (selected: boolean) => `flex items-start gap-2.5 rounded-lg border p-2.5 focus-within:ring-2 focus-within:ring-ring ${selected ? "border-foreground bg-muted/50" : "border-border"}`
      return <fieldset key={question.id} disabled={inactive}>
        <legend className="mb-2 w-full text-sm"><span className="mr-2 text-xs text-muted-foreground">{firstNumber + index + 1}</span><span className="font-medium">{question.prompt}</span><span className="float-right ml-2 text-xs text-muted-foreground">{question.kind === "single" ? "Pick one" : "Pick any"}</span></legend>
        <div className="grid gap-2 sm:grid-cols-2">
          {question.options.map((option, optionIndex) => <label key={optionIndex} className={tileClass(answer.choices.includes(option.label))}>
            <input className="mt-0.5 accent-foreground" type={inputType} name={name} checked={answer.choices.includes(option.label)} onChange={() => {
              update(selectChoice(question, answer, option.label))
              if (question.kind === "single") setOtherSelected((previous) => new Map(previous).set(question.id, false))
            }} />
            <span className="text-sm"><span className="block">{option.label}</span>{option.detail && <span className="mt-1 block text-xs text-muted-foreground">{option.detail}</span>}</span>
          </label>)}
          <div className={tileClass(other)}>
            <input aria-label={`Other: ${question.prompt}`} className="mt-1 accent-foreground" type={inputType} name={name} checked={other} onChange={() => {
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
      <div className="flex gap-2"><Button variant="outline" size="sm" disabled={inactive} onClick={() => advance(true)}>Skip</Button>
        <Button size="sm" disabled={inactive || !stepComplete(questions, answers)} onClick={() => advance(false)}><ArrowRight className="size-3.5" />Continue</Button></div>
    </div>
  </div>
}
