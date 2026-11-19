import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import {
  api,
  clearAuthSession,
  getAuthToken,
  getStoredAdmin,
  setAuthSession,
  UNAUTHORIZED_EVENT,
} from './api'
import type { AdminUser } from './types'

type AuthContextValue = {
  admin: AdminUser | null
  isAuthenticated: boolean
  isChecking: boolean
  login: (username: string, password: string) => Promise<AdminUser>
  logout: () => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient()
  const [admin, setAdmin] = useState<AdminUser | null>(() => (getAuthToken() ? getStoredAdmin() : null))
  const [isChecking, setIsChecking] = useState<boolean>(() => Boolean(getAuthToken()))

  useEffect(() => {
    const token = getAuthToken()
    if (!token) {
      setIsChecking(false)
      setAdmin(null)
      return
    }

    let cancelled = false
    api
      .me()
      .then((me) => {
        if (cancelled) return
        setAuthSession(token, me)
        setAdmin(me)
      })
      .catch(() => {
        if (cancelled) return
        clearAuthSession()
        setAdmin(null)
      })
      .finally(() => {
        if (!cancelled) setIsChecking(false)
      })

    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    const onUnauthorized = () => {
      clearAuthSession()
      setAdmin(null)
      setIsChecking(false)
      queryClient.clear()
    }
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized)
  }, [queryClient])

  const login = useCallback(async (username: string, password: string) => {
    const res = await api.login({ username, password })
    setAuthSession(res.token, res.admin)
    setAdmin(res.admin)
    return res.admin
  }, [])

  const logout = useCallback(() => {
    clearAuthSession()
    setAdmin(null)
    queryClient.clear()
  }, [queryClient])

  const value = useMemo<AuthContextValue>(
    () => ({
      admin,
      isAuthenticated: Boolean(admin && getAuthToken()),
      isChecking,
      login,
      logout,
    }),
    [admin, isChecking, login, logout],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) {
    throw new Error('AuthContext 尚未初始化')
  }
  return ctx
}
