"use client"

import type { ComponentProps, ReactNode } from "react"
import { cn } from "@/lib/utils"

/**
 * Glass composer shell: one translucent surface with a hairline drawn by
 * ::after so the border never doubles up with the drag ring.
 */
export function ComposerShell({
  dragOver,
  className,
  children,
  ...props
}: ComponentProps<"form"> & { dragOver?: boolean }) {
  return (
    <form
      className={cn(
        "group/composer relative isolate mx-auto w-full max-w-3xl rounded-[22px]",
        "bg-[color-mix(in_srgb,var(--card)_var(--glass-opacity),transparent)] backdrop-blur-[var(--glass-blur)] backdrop-saturate-[var(--glass-saturation)]",
        "shadow-[0_12px_28px_-18px_rgb(0_0_0/40%)] dark:shadow-none",
        "after:pointer-events-none after:absolute after:inset-0 after:rounded-[inherit] after:border after:border-black/8 dark:after:border-white/5 dark:after:shadow-[inset_0_1px_rgb(255_255_255/3%)]",
        "transition-colors duration-200",
        !dragOver && "has-[textarea:focus-visible]:ring-2 has-[textarea:focus-visible]:ring-focus-ring",
        dragOver && "bg-accent/45 ring-1 ring-primary/70",
        className
      )}
      {...props}
    >
      {children}
    </form>
  )
}

/** Attached banner drawn above the shell (slash-command toasts). */
export function ComposerBanner({ children }: { children: ReactNode }) {
  return (
    <div className="relative z-0 mx-auto w-full max-w-3xl px-4">
      <div className="-mb-[calc(1rem+1px)] rounded-t-[16px] border border-b-0 border-warning/28 bg-warning-surface px-3 pb-[calc(1rem+1px+0.375rem)] pt-1.5 text-xs text-warning-foreground">
        {children}
      </div>
    </div>
  )
}

/** Round send button with the t3 arrow glyph. */
export function SendButton({ className, size = "md", ...props }: ComponentProps<"button"> & { size?: "sm" | "md" }) {
  return (
    <button
      type="submit"
      className={cn(
        "flex shrink-0 cursor-pointer items-center justify-center rounded-full bg-message-action text-message-action-foreground shadow-xs transition-all duration-150",
        "hover:scale-105 hover:bg-message-action-hover active:shadow-none",
        "enabled:inset-shadow-[0_1px_rgb(255_255_255/16%)]",
        "disabled:cursor-default disabled:opacity-30 disabled:shadow-none disabled:hover:scale-100",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
        size === "md" ? "size-11 sm:size-8" : "size-7",
        className
      )}
      {...props}
    >
      <svg width="14" height="14" viewBox="0 0 14 14" fill="none" aria-hidden="true">
        <path d="M7 11.5V2.5M7 2.5L3 6.5M7 2.5L11 6.5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
      <span className="sr-only">Send message</span>
    </button>
  )
}
