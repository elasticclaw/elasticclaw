import { cn } from "@/lib/utils"

export function WorkflowName({
  name,
  className,
}: {
  name: string
  className?: string
}) {
  return (
    <span
      className={cn("inline-block max-w-full truncate", className)}
      title={name}
    >
      {name}
    </span>
  )
}
