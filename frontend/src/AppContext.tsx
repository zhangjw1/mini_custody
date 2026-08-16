import { createContext, useContext } from 'react'
import type { Asset, User } from './types'

type AppContextValue = {
  users: User[]
  userId: number
  setUserId: (userId: number) => void
  assets: Asset[]
  asset: Asset
  setAssetSymbol: (symbol: string) => void
}

export const AppContext = createContext<AppContextValue | null>(null)

export function useAppContext(): AppContextValue {
  const value = useContext(AppContext)
  if (!value) {
    throw new Error('应用上下文尚未初始化')
  }
  return value
}
