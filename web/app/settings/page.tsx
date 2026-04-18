"use client"

import { useRouter } from "next/navigation"
import { Button } from "@/components/ui/button"
import { ArrowLeft } from "lucide-react"

export default function SettingsPage() {
  const router = useRouter()

  return (
    <div className="flex h-screen bg-background items-center justify-center">
      <div className="w-full max-w-md p-8 space-y-6">
        <div className="flex items-center gap-3">
          <Button variant="ghost" size="icon" onClick={() => router.push("/")}>
            <ArrowLeft className="size-4" />
          </Button>
          <h1 className="text-xl font-semibold">Settings</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Configuration is managed server-side via <code className="bg-muted px-1 rounded text-xs">hub.yaml</code> and environment variables.
        </p>
      </div>
    </div>
  )
}
