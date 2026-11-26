<?php

declare(strict_types=1);

namespace PricingLab;

final class SaleAmount
{
    public static function normalize(int $units): int
    {
        return max(0, $units);
    }
}
