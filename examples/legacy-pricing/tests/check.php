<?php

declare(strict_types=1);

require __DIR__ . '/../app/Calculator.php';
require __DIR__ . '/../app/Currency.php';

$expectations = [
    'sale applies discount' => [\UpgradeLab\Pricing\estimateSale(1200, 250), 950],
    'credit may cross zero' => [\UpgradeLab\Pricing\estimateCredit(300, 500), -200],
    'same-named alternative is unrelated' => [\UpgradeLab\Pricing\diagnostic(), 7],
    'currency contract' => [\UpgradeLab\Pricing\displayAmount(18), '18 cents'],
];

foreach ($expectations as $name => [$actual, $expected]) {
    if ($actual !== $expected) {
        fwrite(STDERR, $name . PHP_EOL);
        exit(1);
    }
}

echo 'Pricing expectations passed', PHP_EOL;
